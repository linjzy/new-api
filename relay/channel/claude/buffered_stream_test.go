package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gin-gonic/gin"
)

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func setClaudeBufferedTestTimeout(t *testing.T, timeout int) {
	t.Helper()
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = timeout
	t.Cleanup(func() {
		constant.StreamingTimeout = previousStreamingTimeout
	})
}

func (b *blockingReadCloser) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingReadCloser) Close() error {
	b.once.Do(func() {
		close(b.closed)
	})
	return nil
}

func TestAdaptorDoResponseBuffersClaudeStreamForNonStreamClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setClaudeBufferedTestTimeout(t, 30)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	clientStream := false
	info := &relaycommon.RelayInfo{
		Request: &dto.ClaudeRequest{
			Stream: &clientStream,
		},
		RelayFormat: types.RelayFormatClaude,
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "claude-test",
		},
	}
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":11,"cache_creation_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"consider"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"OK"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool_1","name":"lookup","input":{}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":2}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader(sse)),
	}

	usageValue, apiErr := (&Adaptor{}).DoResponse(c, resp, info)
	require.Nil(t, apiErr)
	usage, ok := usageValue.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, 11, usage.PromptTokens)
	assert.Equal(t, 9, usage.CompletionTokens)
	assert.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
	assert.False(t, info.IsStream)

	var response dto.ClaudeResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	var responseObject map[string]interface{}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &responseObject))
	stopSequence, exists := responseObject["stop_sequence"]
	assert.True(t, exists)
	assert.Nil(t, stopSequence)
	assert.Equal(t, "msg_test", response.Id)
	assert.Equal(t, "message", response.Type)
	assert.Equal(t, "assistant", response.Role)
	assert.Equal(t, "claude-test", response.Model)
	assert.Equal(t, "tool_use", response.StopReason)
	require.Len(t, response.Content, 3)
	assert.Equal(t, "thinking", response.Content[0].Type)
	require.NotNil(t, response.Content[0].Thinking)
	assert.Equal(t, "consider", *response.Content[0].Thinking)
	assert.Equal(t, "sig", response.Content[0].Signature)
	assert.Equal(t, "text", response.Content[1].Type)
	assert.Equal(t, "OK", response.Content[1].GetText())
	assert.Equal(t, "tool_use", response.Content[2].Type)
	assert.Equal(t, "tool_1", response.Content[2].Id)
	assert.Equal(t, "lookup", response.Content[2].Name)
	assert.Equal(t, map[string]interface{}{"city": "Paris"}, response.Content[2].Input)
	require.NotNil(t, response.Usage)
	assert.Equal(t, 11, response.Usage.InputTokens)
	assert.Equal(t, 2, response.Usage.CacheCreationInputTokens)
	assert.Equal(t, 3, response.Usage.CacheReadInputTokens)
	assert.Equal(t, 9, response.Usage.OutputTokens)
}

func TestAdaptorDoResponseKeepsClaudeStreamForStreamClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = previousStreamingTimeout
	})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	clientStream := true
	info := &relaycommon.RelayInfo{
		Request: &dto.ClaudeRequest{
			Stream: &clientStream,
		},
		RelayFormat: types.RelayFormatClaude,
		IsStream:    true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "claude-test",
		},
	}
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
		},
		Body: io.NopCloser(strings.NewReader(sse)),
	}

	_, apiErr := (&Adaptor{}).DoResponse(c, resp, info)
	require.Nil(t, apiErr)
	assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
	assert.True(t, info.IsStream)
	assert.Contains(t, recorder.Body.String(), `"type":"message_start"`)
}

func TestClaudeBufferedStreamPreservesRedactedThinkingAndCitations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setClaudeBufferedTestTimeout(t, 30)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatClaude,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"},
	}
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_content","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":2,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"encrypted-payload"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"Answer"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"char_location","cited_text":"source","document_index":0,"start_char_index":0,"end_char_index":6}}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}

	_, apiErr := ClaudeBufferedStreamHandler(c, resp, info)
	require.Nil(t, apiErr)
	var response dto.ClaudeResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Content, 2)
	assert.Equal(t, "redacted_thinking", response.Content[0].Type)
	assert.Equal(t, "encrypted-payload", response.Content[0].Data)
	require.Len(t, response.Content[1].Citations, 1)
	var citation map[string]interface{}
	require.NoError(t, common.Unmarshal(response.Content[1].Citations[0], &citation))
	assert.Equal(t, "char_location", citation["type"])
	assert.Equal(t, "source", citation["cited_text"])
}

func TestClaudeBufferedStreamEstimatesMissingUsageAndUpdatesModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setClaudeBufferedTestTimeout(t, 30)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatClaude,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "configured-model"},
	}
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_usage","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[]}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"estimated output"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}

	usage, apiErr := ClaudeBufferedStreamHandler(c, resp, info)
	require.Nil(t, apiErr)
	assert.Equal(t, "claude-3-5-sonnet", info.UpstreamModelName)
	assert.Positive(t, usage.CompletionTokens)

	var response dto.ClaudeResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotNil(t, response.Usage)
	assert.Positive(t, response.Usage.OutputTokens)
}

func TestClaudeBufferedStreamStopsWhenClientCancels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setClaudeBufferedTestTimeout(t, 30)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(requestContext)
	cancel()
	body := newBlockingReadCloser()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatClaude,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"},
	}

	_, apiErr := ClaudeBufferedStreamHandler(c, resp, info)
	require.ErrorIs(t, apiErr, context.Canceled)
	select {
	case <-body.closed:
	default:
		t.Fatal("upstream body was not closed after client cancellation")
	}
}

func TestClaudeBufferedStreamStopsOnIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setClaudeBufferedTestTimeout(t, 0)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := newBlockingReadCloser()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatClaude,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"},
	}

	_, apiErr := ClaudeBufferedStreamHandler(c, resp, info)
	require.Error(t, apiErr)
	assert.Contains(t, apiErr.Error(), "Claude stream timed out")
	select {
	case <-body.closed:
	default:
		t.Fatal("upstream body was not closed after timeout")
	}
}
