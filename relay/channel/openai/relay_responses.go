package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(responsesResponse.Model)
	responseBody = rewriteSGLangResponsesCreatedAt(info, responseBody, "created_at", responsesResponse.CreatedAt)

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := &dto.Usage{}
	service.ApplyResponsesUsage(usage, responsesResponse.Usage)
	// Count actual tool invocations from Output (not tool declarations).
	for _, output := range responsesResponse.Output {
		switch output.Type {
		case dto.BuildInCallWebSearchCall:
			info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
		case dto.BuildInCallFileSearchCall:
			info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
		case dto.BuildInCallFunctionCall:
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
		}
	}

	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	if !relaycommon.IsNonBillableResponsesStatus(responsesResponse.Status) {
		for i := range responsesResponse.Output {
			idx := i
			imageCounter.Observe(&responsesResponse.Output[i], &idx)
		}
	}
	imageCounter.Commit(info)

	return usage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	accumulator := service.NewResponsesUsageAccumulator(info)
	var streamErr *types.NewAPIError
	var prelude []dto.ResponsesStreamResponse
	var preludeData []string
	preludeBytes := 0
	terminalSeen := false
	firstOutputEvent := ""
	firstOutputBytes := 0
	info.ResponsesStreamError = nil
	info.ResponsesCapacityFailure = false
	common.SetContextKey(c, constant.ContextKeyResponsesStreamFailed, false)
	info.ReceivedResponseCount = 0

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			streamErr = types.NewErrorWithStatusCode(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			sr.Stop(streamErr)
			return
		}
		if streamResponse.Response != nil {
			data = string(rewriteSGLangResponsesCreatedAt(info, []byte(data), "response.created_at", streamResponse.Response.CreatedAt))
		}
		// Accounting runs before the commit decision: usage carried by a terminal
		// failure counts as work that must not be replayed.
		accumulator.Observe(&streamResponse)

		failure, capacityFailure := responsesStreamFailure(streamResponse)
		if failure != nil {
			terminalSeen = true
			streamErr = failure
			info.ResponsesCapacityFailure = capacityFailure
			// A billed response may have executed upstream work, even if no delta
			// reached us. Commit it and preserve its usage instead of replaying it.
			if c.Writer.Written() || accumulator.Usage().TotalTokens > 0 || responsesFailureHasWork(data) {
				if !c.Writer.Written() {
					helper.StartEventStream(c, resp)
					for i, event := range prelude {
						if err := helper.ResponseChunkData(c, event, preludeData[i]); err != nil {
							sr.Stop(err)
							return
						}
					}
				}
				_ = helper.ResponseChunkData(c, streamResponse, data)
			}
			sr.Stop(streamErr)
			return
		}
		switch streamResponse.Type {
		case "response.completed", "response.done", "response.incomplete",
			"response.cancelled", "response.canceled":
			terminalSeen = true
		}

		// Provider keepalives carry no generated work. Drop them before output
		// instead of committing SSE or spending the bounded lifecycle buffer.
		// The scanner's absolute prelude deadline still bounds the wait.
		if !c.Writer.Written() && responsesKeepaliveEvent(streamResponse.Type, data) {
			return
		}

		// Empty message/reasoning item and text-part placeholders are also
		// initialization. Text, encrypted reasoning, tool and unknown events
		// commit immediately; only explicitly empty placeholders can be withheld.
		// Codex lifecycle events echo instructions and other large metadata;
		// created + in_progress can exceed 64 KiB before producing any output.
		if !c.Writer.Written() && responsesPreludeEvent(streamResponse.Type, data) && len(prelude) < 16 && preludeBytes+len(data) <= 2<<20 {
			prelude = append(prelude, streamResponse)
			preludeData = append(preludeData, data)
			preludeBytes += len(data)
			return
		}
		if !c.Writer.Written() {
			firstOutputEvent = streamResponse.Type
			firstOutputBytes = len(data)
			helper.StartEventStream(c, resp)
			for i, event := range prelude {
				if err := helper.ResponseChunkData(c, event, preludeData[i]); err != nil {
					streamErr = types.NewErrorWithStatusCode(err, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
					sr.Stop(streamErr)
					return
				}
			}
			prelude = nil
			preludeData = nil
		}
		if err := helper.ResponseChunkData(c, streamResponse, data); err != nil {
			streamErr = types.NewErrorWithStatusCode(err, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			sr.Stop(streamErr)
			return
		}
	}, helper.StreamScannerOptions{DeferHeaders: true})

	if streamErr == nil && !terminalSeen {
		message := "response stream ended before completion"
		statusCode := http.StatusBadGateway
		if info.StreamStatus.EndReason == relaycommon.StreamEndReasonTimeout {
			message = "response stream timed out before completion"
			statusCode = http.StatusGatewayTimeout
		}
		streamErr = types.NewErrorWithStatusCode(fmt.Errorf("%s", message), types.ErrorCodeBadResponse, statusCode, types.ErrOptionWithSkipRetry())
	}
	if streamErr != nil {
		logger.LogWarn(c, fmt.Sprintf("Responses stream failure: committed=%t first_output_event=%q first_output_bytes=%d prelude_bytes=%d capacity=%t", c.Writer.Written(), firstOutputEvent, firstOutputBytes, preludeBytes, info.ResponsesCapacityFailure))
		info.StreamStatus.SetUpstreamError(streamErr)
		info.ResponsesStreamError = streamErr
		common.SetContextKey(c, constant.ContextKeyResponsesStreamFailed, true)
		if c.Writer.Written() || c.Request.Context().Err() != nil {
			types.ErrOptionWithSkipRetry()(streamErr)
		}
	}

	common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, info.StreamStatus)
	info.StreamStatus.RequireTerminal()
	return accumulator.Finish(), streamErr
}

func rewriteSGLangResponsesCreatedAt(info *relaycommon.RelayInfo, payload []byte, path string, createdAt dto.IntValue) []byte {
	if info.GetChannelType() != constant.ChannelTypeSGLang {
		return payload
	}
	if !gjson.GetBytes(payload, path).Exists() {
		return payload
	}
	patched, err := sjson.SetBytes(payload, path, int(createdAt))
	if err != nil {
		return payload
	}
	return patched
}

func responsesKeepaliveEvent(eventType, data string) bool {
	if eventType != "keepalive" {
		return false
	}
	var event map[string]any
	if common.UnmarshalJsonStr(data, &event) != nil {
		return false
	}
	for key, value := range event {
		switch key {
		case "type":
		case "sequence_number", "timestamp":
			if number, ok := value.(float64); !ok || number < 0 {
				return false
			}
		default:
			// Do not hide usage, content or unknown provider extensions.
			return false
		}
	}
	return true
}

func responsesPreludeEvent(eventType, data string) bool {
	switch eventType {
	case "response.created", "response.in_progress", "response.queued", "response.output_item.added", "response.content_part.added", "response.reasoning_summary_part.added":
	default:
		return false
	}
	var event map[string]any
	if common.UnmarshalJsonStr(data, &event) != nil {
		return false
	}
	switch eventType {
	case "response.output_item.added":
		return responsesEmptyOutputItem(event["item"])
	case "response.content_part.added", "response.reasoning_summary_part.added":
		return responsesEmptyTextPart(event["part"])
	default:
		if event["response"] == nil {
			return true
		}
		response, ok := event["response"].(map[string]any)
		return ok && responsesZeroValue(response["usage"]) && responsesEmptyOutput(response["output"])
	}
}

func responsesFailureHasWork(data string) bool {
	var raw map[string]any
	if common.UnmarshalJsonStr(data, &raw) != nil {
		return true
	}
	if raw["response"] == nil {
		return false
	}
	response, ok := raw["response"].(map[string]any)
	// A provider may report detail-only usage or new billable fields that the
	// shared DTO does not retain. Only an explicitly empty usage is replayable.
	return !ok || !responsesZeroValue(response["usage"]) || !responsesEmptyOutput(response["output"])
}

func responsesEmptyOutput(value any) bool {
	if value == nil {
		return true
	}
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if !responsesEmptyOutputItem(item) {
			return false
		}
	}
	return true
}

// Inspect raw fields: the shared DTO does not retain encrypted_content and
// future item extensions. Unknown populated fields must prevent replay.
func responsesEmptyOutputItem(value any) bool {
	item, ok := value.(map[string]any)
	if !ok || (item["type"] != "message" && item["type"] != "reasoning") {
		return false
	}
	for key, value := range item {
		switch key {
		case "id", "type", "role", "phase":
		case "status":
			if value != nil && value != "" && value != "in_progress" {
				return false
			}
		case "content", "summary":
			if value == nil {
				continue
			}
			parts, ok := value.([]any)
			if !ok {
				return false
			}
			for _, part := range parts {
				if !responsesEmptyTextPart(part) {
					return false
				}
			}
		case "encrypted_content":
			if value != nil && value != "" {
				return false
			}
		default:
			if value != nil {
				return false
			}
		}
	}
	return true
}

func responsesEmptyTextPart(value any) bool {
	part, ok := value.(map[string]any)
	if !ok || (part["type"] != "output_text" && part["type"] != "summary_text" && part["type"] != "reasoning_text") {
		return false
	}
	for key, value := range part {
		switch key {
		case "type":
		case "text":
			if value != nil && value != "" {
				return false
			}
		case "annotations", "logprobs":
			if value != nil {
				values, ok := value.([]any)
				if !ok || len(values) > 0 {
					return false
				}
			}
		default:
			if value != nil {
				return false
			}
		}
	}
	return true
}

func responsesZeroValue(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case float64:
		return value == 0
	case map[string]any:
		for _, field := range value {
			if !responsesZeroValue(field) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// responsesStreamFailure classifies protocol failures, independently of the
// successful HTTP handshake. Only explicit transient capacity errors can replay.
func responsesStreamFailure(event dto.ResponsesStreamResponse) (*types.NewAPIError, bool) {
	var upstreamError *types.OpenAIError
	if event.Response != nil {
		upstreamError = event.Response.GetOpenAIError()
	}
	if upstreamError == nil {
		upstreamError = dto.GetOpenAIError(event.Error)
	}
	responseStatus := ""
	if event.Response != nil && len(event.Response.Status) > 0 {
		_ = common.Unmarshal(event.Response.Status, &responseStatus)
	}
	failed := event.Type == "response.failed" || event.Type == "response.error" || event.Type == "error" || responseStatus == "failed"
	if upstreamError == nil && !failed {
		return nil, false
	}
	if upstreamError == nil {
		upstreamError = &types.OpenAIError{Code: event.Code, Message: event.Message, Param: event.Param}
	}
	if upstreamError.Message == "" {
		upstreamError.Message = "upstream response failed"
	}
	code := strings.ToLower(fmt.Sprint(upstreamError.Code))
	capacity := code == "model_at_capacity" || code == "server_is_overloaded" || code == "server_overloaded" || code == "slow_down" || code == "overloaded_error" || code == "rate_limit_exceeded"
	// Some compatible providers omit error.code. Do not override a specific
	// request/auth/policy error based on its message.
	if code == "" || code == "<nil>" {
		message := strings.ToLower(upstreamError.Message)
		capacity = strings.Contains(message, "selected model is at capacity") || strings.Contains(message, "our servers are currently overloaded")
	}
	if capacity {
		return types.WithOpenAIError(*upstreamError, http.StatusServiceUnavailable), true
	}
	return types.WithOpenAIError(*upstreamError, http.StatusBadGateway, types.ErrOptionWithSkipRetry()), false
}
