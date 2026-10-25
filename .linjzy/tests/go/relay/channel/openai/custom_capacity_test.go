package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type customCancelAtHeadersWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func TestCustomResponsesTerminalPreservesVendorToolUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	for _, tc := range []struct {
		name, eventType, status, errorJSON string
		failed                             bool
	}{
		{name: "completed response", eventType: "response.completed", status: "completed", errorJSON: "null"},
		{name: "billed capacity failure", eventType: "response.failed", status: "failed", errorJSON: `{"code":"model_at_capacity","message":"capacity"}`, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			info := &relaycommon.RelayInfo{
				OriginModelName: "review-model",
				DisablePing:     true,
				ChannelMeta:     &relaycommon.ChannelMeta{UpstreamModelName: "review-model"},
				VendorToolUsage: func(raw []byte) map[string]int {
					return map[string]int{"web_search": int(gjson.GetBytes(raw, "response.usage.num_search_queries").Int())}
				},
			}
			body := fmt.Sprintf("data: {\"type\":%q,\"response\":{\"status\":%q,\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12,\"num_search_queries\":3},\"error\":%s}}\n\n", tc.eventType, tc.status, tc.errorJSON)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
			require.NotNil(t, usage)
			assert.Equal(t, 12, usage.TotalTokens)
			require.NotNil(t, info.ResponsesUsageInfo)
			require.Contains(t, info.BuiltInTools, "web_search")
			assert.Equal(t, 3, info.BuiltInTools["web_search"].CallCount)
			if tc.failed {
				require.NotNil(t, apiErr)
				assert.True(t, types.IsSkipRetryError(apiErr))
			} else {
				assert.Nil(t, apiErr)
			}
		})
	}
}

func TestCustomResponsesTransientFailureClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	for _, tc := range []struct {
		name, errorJSON string
		retry           bool
	}{
		{"account concurrency", `{"code":"gateway_concurrency_limit","message":"concurrency limit"}`, true},
		{"explicitly retryable stream break", `{"code":"upstream_stream_break","message":"interrupted","retryable":true}`, true},
		{"safe stream break message", `{"code":"upstream_stream_break","message":"Upstream stream ended prematurely; safe to retry"}`, true},
		{"nonretryable flag wins", `{"code":"upstream_stream_break","message":"Upstream stream ended prematurely; safe to retry","retryable":false}`, false},
		{"unmarked break", `{"code":"upstream_stream_break","message":"stream interrupted"}`, false},
		{"generic temporary failure", `{"code":"upstream_error","message":"The service is temporarily unavailable. Please retry later."}`, true},
		{"generic overload", `{"code":"server_error","message":"Our servers are currently overloaded. Please try again later."}`, true},
		{"unknown service failure", `{"code":"unknown_error","message":"Upstream service temporarily unavailable"}`, true},
		{"invalid request is not transient", `{"code":"bad_request","message":"The service is temporarily unavailable. Please retry later."}`, false},
		{"authentication type wins", `{"code":"unknown_error","type":"authentication_error","message":"The service is temporarily unavailable. Please retry later."}`, false},
		{"usage limit is not capacity", `{"code":"usage_limit_reached","message":"Please try again later."}`, false},
		{"arbitrary retry message", `{"code":"unknown_error","message":"Please try again later."}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, envelope := range []string{
				`{"type":"response.failed","response":{"status":"failed","output":[],"usage":null,"error":%s}}`,
				`{"type":"error","error":%s}`,
			} {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				info := &relaycommon.RelayInfo{OriginModelName: "review-model", DisablePing: true, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "review-model"}}
				body := "data: " + fmt.Sprintf(envelope, tc.errorJSON) + "\n\n"
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
				require.NotNil(t, apiErr)
				require.NotNil(t, usage)
				assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode, "keep the original public failure status")
				assert.False(t, c.Writer.Written())
				assert.Zero(t, usage.TotalTokens)
				assert.Equal(t, tc.retry, info.ResponsesCapacityFailure)
				assert.Equal(t, !tc.retry, types.IsSkipRetryError(apiErr))
			}
		})
	}
}

func (w *customCancelAtHeadersWriter) Header() http.Header {
	w.cancel()
	return w.ResponseRecorder.Header()
}

func TestCustomResponsesCapacityBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	for _, tc := range []struct {
		name            string
		body            string
		cancelAtHeaders bool
		written         bool
		tokens          int
		skipRetry       bool
	}{
		{
			name: "empty lifecycle and keepalive permit capacity failover",
			body: "data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\",\"output\":[],\"usage\":null}}\n\n" +
				"data: {\"type\":\"keepalive\",\"timestamp\":1}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":null,\"error\":{\"code\":\"model_at_capacity\",\"message\":\"capacity\"}}}\n\n",
		},
		{
			name:      "billed failure commits stream and prevents replay",
			body:      "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12},\"error\":{\"code\":\"model_at_capacity\",\"message\":\"capacity\"}}}\n\n",
			written:   true,
			tokens:    12,
			skipRetry: true,
		},
		{
			name: "usage from an earlier event survives a failure without usage",
			body: "data: {\"type\":\"response.in_progress\",\"response\":{\"status\":\"in_progress\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":null,\"error\":{\"code\":\"model_at_capacity\",\"message\":\"capacity\"}}}\n\n",
			written:   true,
			tokens:    12,
			skipRetry: true,
		},
		{
			name:      "response error envelope preserves its reported usage",
			body:      "data: {\"type\":\"response.error\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12},\"error\":{\"code\":\"model_at_capacity\",\"message\":\"capacity\"}}}\n\n",
			written:   true,
			tokens:    12,
			skipRetry: true,
		},
		{
			name:            "disconnect during header setup leaves known billable usage with unwritten response",
			body:            "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12},\"error\":{\"code\":\"model_at_capacity\",\"message\":\"capacity\"}}}\n\n",
			cancelAtHeaders: true,
			tokens:          12,
			skipRetry:       true,
		},
		{
			name: "generated tool arguments prevent replay",
			body: "data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{}\"}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"model_at_capacity\",\"message\":\"capacity\"}}}\n\n",
			written:   true,
			tokens:    1,
			skipRetry: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var writer http.ResponseWriter = httptest.NewRecorder()
			if tc.cancelAtHeaders {
				writer = &customCancelAtHeadersWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
			}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			info := &relaycommon.RelayInfo{
				OriginModelName: "review-model",
				DisablePing:     true,
				ChannelMeta:     &relaycommon.ChannelMeta{UpstreamModelName: "review-model"},
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
			usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
			require.NotNil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
			assert.Equal(t, tc.written, c.Writer.Written())
			assert.Equal(t, tc.tokens, usage.TotalTokens)
			assert.Equal(t, tc.skipRetry, types.IsSkipRetryError(apiErr))
		})
	}
}
