package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	hostdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCustomCapacityFailoverContract(t *testing.T) {
	db := modelManagementDB(t, "sqlite", "")
	// This fixture restores global database/Redis state at teardown; keep
	// unrelated asynchronous performance collection outside the relay contract.
	oldMetrics := config.GlobalConfig.ExportAllConfigs()["perf_metrics_setting.enabled"]
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "false"}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": oldMetrics}))
	})
	oldRetries, oldTimeout, oldLog := common.RetryTimes, constant.StreamingTimeout, constant.ErrorLogEnabled
	common.RetryTimes, constant.StreamingTimeout, constant.ErrorLogEnabled = 2, 30, false
	t.Cleanup(func() {
		common.RetryTimes, constant.StreamingTimeout, constant.ErrorLogEnabled = oldRetries, oldTimeout, oldLog
	})
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"capacity-fixture":0}`))
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.UserSubscription{}))
	user := model.User{Id: 780001, Username: "capacity-fixture", Group: "default", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	for _, tc := range []struct {
		name             string
		fallback, pinned bool
		wantCalls        int
	}{
		{"alternative succeeds", true, false, 2},
		{"no alternative preserves original failure", false, false, 1},
		{"pinned request stays on original channel", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := r.Header.Get("Authorization")
				calls = append(calls, key)
				w.Header().Set("Content-Type", "text/event-stream")
				if key == "Bearer first" {
					fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":null,\"error\":{\"code\":\"gateway_concurrency_limit\",\"message\":\"account concurrency limit\"}}}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
				}
			}))
			defer upstream.Close()
			channels := []model.Channel{
				{Id: 781, Name: "first", Type: constant.ChannelTypeOpenAI, Key: "first", BaseURL: &upstream.URL, Models: "capacity-fixture", Group: "default", Status: common.ChannelStatusEnabled, Priority: common.GetPointer(int64(10))},
				{Id: 782, Name: "second", Type: constant.ChannelTypeOpenAI, Key: "second", BaseURL: &upstream.URL, Models: "capacity-fixture", Group: "default", Status: common.ChannelStatusEnabled, Priority: common.GetPointer(int64(5))},
			}
			if !tc.fallback {
				channels[1].Status = common.ChannelStatusManuallyDisabled
			}
			for i := range channels {
				require.NoError(t, db.Save(&channels[i]).Error)
				require.NoError(t, channels[i].UpdateAbilities(nil))
			}
			common.MemoryCacheEnabled = true
			model.InitChannelCache()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"capacity-fixture","input":"hello","stream":true}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("id", user.Id)
			c.Set("group", "default")
			c.Set("token_group", "default")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
			require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channels[0], "capacity-fixture"))
			if tc.pinned {
				service.GetChannelConstraints(c).AddPin(hostdto.ChannelPin{ChannelId: 781, Source: hostdto.PinSourceToken, Rank: hostdto.PinRankToken, RetryMode: hostdto.PinRetrySingleAttempt})
			}
			Relay(c, types.RelayFormatOpenAIResponses)
			assert.Len(t, calls, tc.wantCalls, w.Body.String())
			if tc.wantCalls == 2 {
				assert.Equal(t, []string{"Bearer first", "Bearer second"}, calls)
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), "response.completed")
				assert.NotContains(t, w.Body.String(), "gateway_concurrency_limit")
			} else {
				assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), "gateway_concurrency_limit")
				assert.NotContains(t, w.Body.String(), "get_channel_failed")
			}
		})
	}
}
