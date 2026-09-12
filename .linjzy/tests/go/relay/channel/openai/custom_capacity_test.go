package openai

import (
	"context"
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
)

type customCancelAtHeadersWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
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
			name:            "disconnect during header setup leaves known billable usage with unwritten response",
			body:            "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12},\"error\":{\"code\":\"model_at_capacity\",\"message\":\"capacity\"}}}\n\n",
			cancelAtHeaders: true,
			tokens:          12,
			skipRetry:       true,
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
