package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type customSettlement struct{ charges []int }

func (s *customSettlement) Settle(q int) error       { s.charges = append(s.charges, q); return nil }
func (s *customSettlement) Refund(*gin.Context)      {}
func (s *customSettlement) NeedsRefund() bool        { return len(s.charges) == 0 }
func (s *customSettlement) GetPreConsumedQuota() int { return 100 }
func (s *customSettlement) Reserve(int) error        { return nil }

type customDisconnectedWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	ready  func() bool
}

func (w *customDisconnectedWriter) Header() http.Header {
	if w.ready != nil && w.ready() {
		w.cancel()
	}
	return w.ResponseRecorder.Header()
}

func TestCustomResponsesSettlesUsageBeforeFirstClientWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	oldDB, oldLog := model.DB, model.LOG_DB
	oldBatch, oldLogEnabled := common.BatchUpdateEnabled, common.LogConsumeEnabled
	model.DB, model.LOG_DB = db, db
	common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLog
		common.BatchUpdateEnabled, common.LogConsumeEnabled = oldBatch, oldLogEnabled
		_ = sqlDB.Close()
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Channel{}))
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "custom-billing", Quota: 1000}).Error)
	require.NoError(t, db.Create(&model.Channel{Id: 1, Name: "custom-billing", Status: common.ChannelStatusEnabled}).Error)
	for _, tc := range []struct {
		name, usage string
		billed      bool
	}{
		{"known usage survives disconnect", `{"input_tokens":10,"output_tokens":2,"total_tokens":12}`, true},
		{"unbilled capacity failure keeps precharge refundable", `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"status":"failed","output":[],"usage":` + tc.usage + `,"error":{"code":"model_at_capacity","message":"capacity"}}}` + "\n\n"))
			}))
			defer upstream.Close()
			reqctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &customDisconnectedWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(reqctx)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyChannelId, 1)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream.URL)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "test-key")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "custom-model")
			bill := &customSettlement{}
			stream := true
			info := &relaycommon.RelayInfo{UserId: 1, UserQuota: 1000000000, OriginModelName: "custom-model", StartTime: time.Now(), DisablePing: true,
				RelayMode: relayconstant.RelayModeResponses, RelayFormat: types.RelayFormatOpenAIResponses, IsStream: true,
				Request: &dto.OpenAIResponsesRequest{Model: "custom-model", Stream: &stream}, Billing: bill,
				PriceData: hosttypes.PriceData{ModelRatio: 1, CompletionRatio: 1, GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1}},
			}
			w.ready = func() bool { return info.ResponsesCapacityFailure }
			apiErr := ResponsesHelper(c, info)
			require.NotNil(t, apiErr)
			assert.False(t, c.Writer.Written())
			if tc.billed {
				assert.Equal(t, []int{12}, bill.charges)
				assert.False(t, bill.NeedsRefund())
				assert.True(t, types.IsSkipRetryError(apiErr))
			} else {
				assert.Empty(t, w.ResponseRecorder.Header().Get("Content-Type"))
				assert.Empty(t, bill.charges)
				assert.True(t, bill.NeedsRefund())
				assert.False(t, types.IsSkipRetryError(apiErr))
			}
		})
	}
}
