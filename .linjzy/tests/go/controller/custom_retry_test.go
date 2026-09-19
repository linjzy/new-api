package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type customRetryTransport func(*http.Request) (*http.Response, error)

func (f customRetryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCustomSequentialRetryStopsAfterClientBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()
	for index, boundary := range []string{"open", "cancelled", "committed"} {
		t.Run(boundary, func(t *testing.T) {
			db := modelManagementDB(t, "sqlite", "")
			previousPerf := config.GlobalConfig.ExportAllConfigs()["perf_metrics_setting.enabled"]
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": "false"}))
			t.Cleanup(func() {
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"perf_metrics_setting.enabled": previousPerf}))
			})
			previousRetries, previousLog, previousCount := common.RetryTimes, constant.ErrorLogEnabled, constant.CountToken
			previousFree := operation_setting.GetQuotaSetting().EnableFreeModelPreConsume
			previousNotifyLimit := constant.NotifyLimitCount
			fetch := system_setting.GetFetchSetting()
			previousFetch := *fetch
			common.RetryTimes, constant.ErrorLogEnabled, constant.CountToken = 0, false, false
			constant.NotifyLimitCount = 100
			fetch.EnableSSRFProtection = false
			operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = false
			t.Cleanup(func() {
				common.RetryTimes, constant.ErrorLogEnabled, constant.CountToken = previousRetries, previousLog, previousCount
				operation_setting.GetQuotaSetting().EnableFreeModelPreConsume = previousFree
				constant.NotifyLimitCount = previousNotifyLimit
				*fetch = previousFetch
			})
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"custom-retry":0}`))
			// Keep retirement notifications inside this fixture and join them before
			// the database and global configuration are restored.
			notified := make(chan struct{}, 2)
			notify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				notified <- struct{}{}
			}))
			t.Cleanup(notify.Close)
			settings, err := common.Marshal(dto.UserSetting{NotifyType: dto.NotifyTypeWebhook, WebhookUrl: notify.URL})
			require.NoError(t, err)
			require.NoError(t, db.Create(&model.User{Id: 5091900 + index, Username: "custom-retry-root", Role: common.RoleRootUser, Setting: string(settings)}).Error)
			channel := model.Channel{Name: "custom-retry", Type: constant.ChannelTypeOpenAI, Key: "first\nsecond", Models: "custom-retry", Group: "default", Status: common.ChannelStatusEnabled, AutoBan: common.GetPointer(1), BaseURL: common.GetPointer("https://upstream.test"),
				ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeSequential}}
			require.NoError(t, channel.Insert())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"custom-retry","input":"hi","stream":true}`)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, "custom-retry"))
			defer common.CleanupBodyStorage(c)
			client, err := service.GetHttpClientWithProxySettings("", dto.ChannelSettings{})
			require.NoError(t, err)
			originalTransport := client.Transport
			t.Cleanup(func() { client.Transport = originalTransport })
			var keys []string
			client.Transport = customRetryTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "upstream.test" {
					return originalTransport.RoundTrip(r)
				}
				keys = append(keys, r.Header.Get("Authorization"))
				if boundary == "cancelled" {
					cancel()
				} else if boundary == "committed" {
					c.Writer.WriteHeaderNow()
				}
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_api_key","message":"invalid credential"}}`)), Request: r}, nil
			})
			Relay(c, types.RelayFormatOpenAIResponses)
			want := []string{"Bearer first"}
			if boundary == "open" {
				want = append(want, "Bearer second")
			}
			assert.Equal(t, want, keys)
			assert.Len(t, c.GetStringSlice("use_channel"), len(want))
			assert.Equal(t, len(want), service.RequestPolicy(c).Attempts)
			for range len(keys) {
				select {
				case <-notified:
				case <-time.After(5 * time.Second):
					t.Fatal("retirement notification did not finish")
				}
			}
		})
	}
}
