package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const astraIQQuestion = `在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）
苹果味 桃子味 西瓜味
圆形 7 9 8
五角星形 7 6 4

请只输出最终答案的一个整数，不要输出解释、分析过程或其他内容。`

var astraIQAnswerPattern = regexp.MustCompile(`^(?:(?:答案|最少|至少)(?:是|为)?[:：]?\s*)?21(?:\s*(?:个|颗)(?:糖果)?)?[。.!！]?$`)

var astraIQEmptyAnswer = errors.New("empty_answer")

func setAstraIQPrompt(request dto.Request, prompt string) error {
	switch r := request.(type) {
	case *dto.GeneralOpenAIRequest:
		r.ReasoningEffort = "medium"
		r.Messages = []dto.Message{{Role: "user", Content: prompt}}
		r.MaxTokens = nil
		r.MaxCompletionTokens = lo.ToPtr(uint(2048))
	case *dto.OpenAIResponsesRequest:
		r.Reasoning = &dto.Reasoning{Effort: "medium"}
		input, err := common.Marshal([]dto.Message{{Role: "user", Content: prompt}})
		if err != nil {
			return err
		}
		r.Input = input
		r.MaxOutputTokens = lo.ToPtr(uint(2048))
	default:
		return errors.New("Astra IQ requires a chat or Responses endpoint")
	}
	return nil
}

// Apply after channel parameter overrides so a channel's high/low override
// cannot silently change the benchmark. Ordinary requests never call this.
func enforceAstraIQReasoning(body []byte) ([]byte, error) {
	var request map[string]any
	if err := common.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if _, responses := request["input"]; responses {
		reasoning, _ := request["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		reasoning["effort"] = "medium"
		request["reasoning"] = reasoning
		delete(request, "reasoning_effort")
	} else if _, chat := request["messages"]; chat {
		request["reasoning_effort"] = "medium"
		if reasoning, ok := request["reasoning"].(map[string]any); ok {
			reasoning["effort"] = "medium"
		}
	} else {
		return nil, errors.New("unsupported_iq_reasoning_protocol")
	}
	return common.Marshal(request)
}

// Grade only completed assistant output; never match numbers in reasoning,
// usage, prompt echoes, tool calls, incomplete streams or error payloads.
func astraIQAnswer(body []byte) (string, error) {
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "data:") || strings.HasPrefix(text, "event:") {
		var content strings.Builder
		var completedItems strings.Builder
		completed := false
		var responseAnswer string
		for line := range strings.SplitSeq(text, "\n") {
			data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
			if !ok {
				continue
			}
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				continue
			}
			if !gjson.Valid(data) {
				return "", errors.New("invalid_stream")
			}
			event := gjson.Parse(data)
			if (event.Get("error").Exists() && event.Get("error").Type != gjson.Null) || event.Get("type").String() == "error" || event.Get("type").String() == "response.failed" || event.Get("type").String() == "response.incomplete" {
				return "", errors.New("upstream_error")
			}
			if event.Get("type").String() == "response.output_item.done" {
				item := event.Get("item")
				if item.Get("type").String() == "message" && item.Get("role").String() == "assistant" && item.Get("status").String() == "completed" && item.Get("phase").String() != "commentary" {
					for _, part := range item.Get("content").Array() {
						if part.Get("type").String() == "output_text" {
							completedItems.WriteString(part.Get("text").String())
						}
					}
				}
			}
			if event.Get("type").String() == "response.completed" {
				answer, err := astraIQAnswer([]byte(event.Get("response").Raw))
				// Some Responses providers omit output from response.completed.
				// A completed assistant item is authoritative only after the
				// response itself has completed successfully; deltas never pass.
				if errors.Is(err, astraIQEmptyAnswer) && strings.TrimSpace(completedItems.String()) != "" {
					answer, err = strings.TrimSpace(completedItems.String()), nil
				}
				if err != nil {
					return "", err
				}
				responseAnswer, completed = answer, true
			}
			for _, choice := range event.Get("choices").Array() {
				if choice.Get("index").Int() != 0 {
					continue
				}
				if part := choice.Get("delta.content"); part.Type == gjson.String {
					content.WriteString(part.String())
				}
				if finish := choice.Get("finish_reason").String(); finish != "" {
					if finish != "stop" {
						return "", errors.New("incomplete_answer")
					}
					completed = true
				}
			}
		}
		if !completed {
			return "", errors.New("incomplete_stream")
		}
		if responseAnswer != "" {
			return responseAnswer, nil
		}
		if strings.TrimSpace(content.String()) == "" {
			return "", astraIQEmptyAnswer
		}
		return strings.TrimSpace(content.String()), nil
	}
	if !gjson.Valid(text) {
		return "", errors.New("invalid_response")
	}
	response := gjson.Parse(text)
	if response.Get("error").Exists() && response.Get("error").Type != gjson.Null {
		return "", errors.New("upstream_error")
	}
	if choices := response.Get("choices").Array(); len(choices) > 0 {
		choice := choices[0]
		if choice.Get("finish_reason").String() != "stop" {
			return "", errors.New("incomplete_answer")
		}
		answer := choice.Get("message.content")
		if answer.Type != gjson.String || strings.TrimSpace(answer.String()) == "" {
			return "", astraIQEmptyAnswer
		}
		return strings.TrimSpace(answer.String()), nil
	}
	if response.Get("status").String() != "completed" {
		return "", errors.New("incomplete_response")
	}
	var answer strings.Builder
	for _, item := range response.Get("output").Array() {
		if item.Get("type").String() != "message" || item.Get("role").String() != "assistant" {
			continue
		}
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() == "output_text" {
				answer.WriteString(part.Get("text").String())
			}
		}
	}
	if strings.TrimSpace(answer.String()) == "" {
		return "", astraIQEmptyAnswer
	}
	return strings.TrimSpace(answer.String()), nil
}

func probeAstraIQChannel(ctx context.Context, ch *model.Channel, userID int) model.AstraIQResult {
	started := time.Now()
	result := model.AstraIQResult{Status: "error", Detail: "no_enabled_keys"}
	result.Sample.StartedAt = started.Unix()
	result.Sample.Endpoint = "/v1/responses"
	result.Sample.Client = model.AstraIQClient
	if !model.ChannelEligibleForAstraIQ(ch) {
		result.Detail = "channel_disabled"
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	keys := []string{ch.Key}
	if ch.ChannelInfo.IsMultiKey {
		keys = ch.GetKeys()
	}
	for index, key := range keys {
		if ch.ChannelInfo.IsMultiKey {
			if status, exists := ch.ChannelInfo.MultiKeyStatusList[index]; exists && status != common.ChannelStatusEnabled {
				continue
			}
		}
		settings, err := model.GetAstraIQSettings()
		if err != nil {
			result.Status, result.Detail = "pending", "settings_unavailable"
			return result
		}
		if !settings.Active(time.Now()) {
			// A partially checked multi-key channel must not become a pass.
			// Finish in-flight requests, but do not start another after the window.
			result.Status, result.Detail = "pending", "outside_window"
			return result
		}
		// Probe every enabled key without advancing the production polling cursor.
		probe := *ch
		probe.Key, probe.Keys, probe.ChannelInfo = key, nil, model.ChannelInfo{}
		attemptStarted := time.Now()
		test := testAstraIQWithCodex(ctx, &probe, userID)
		attempt := test.iqAttempt
		attempt.KeyIndex, attempt.LatencyMS, attempt.Status = index+1, time.Since(attemptStarted).Milliseconds(), "error"
		if test.localErr != nil || test.newAPIError != nil {
			result.Status, result.Detail = "error", "request_failed"
			if test.iqAttempt.Detail != "" {
				result.Detail = test.iqAttempt.Detail
			}
			if ctx.Err() != nil {
				result.Detail = "timeout"
			}
			message := ""
			if test.localErr != nil {
				message = test.localErr.Error()
			}
			apiError := test.newAPIError
			if apiError == nil {
				_ = errors.As(test.localErr, &apiError)
			}
			if apiError != nil {
				// Use the parsed error fields, not Error(), which can append the
				// complete HTTP body and unrelated upstream request metadata.
				upstream := apiError.ToOpenAIError()
				message = upstream.Message
				if upstream.Code != nil {
					attempt.ErrorCode = astraIQSafeErrorText(fmt.Sprint(upstream.Code), &probe, 128)
				}
				attempt.ErrorType = astraIQSafeErrorText(upstream.Type, &probe, 128)
			}
			attempt.ErrorMessage = astraIQSafeErrorText(message, &probe, 2048)
			attempt.Detail = result.Detail
			result.Sample.Attempts = append(result.Sample.Attempts, attempt)
			break
		}
		answer, err := astraIQAnswer(test.responseBody)
		if err != nil {
			result.Status, result.Detail = "error", err.Error()
			attempt.Detail = result.Detail
			result.Sample.Attempts = append(result.Sample.Attempts, attempt)
			break
		}
		attempt.Response = string([]rune(answer)[:min(len([]rune(answer)), 8192)])
		result.Answer = string([]rune(answer)[:min(len([]rune(answer)), 128)])
		if !astraIQAnswerPattern.MatchString(strings.Trim(answer, "`* \r\n\t")) {
			result.Status, result.Detail = "fail", "wrong_answer"
			attempt.Status, attempt.Detail = result.Status, result.Detail
			result.Sample.Attempts = append(result.Sample.Attempts, attempt)
			break
		}
		result.Status, result.Detail = "pass", ""
		attempt.Status = result.Status
		result.Sample.Attempts = append(result.Sample.Attempts, attempt)
	}
	result.CheckedAt, result.LatencyMS = common.GetTimestamp(), time.Since(started).Milliseconds()
	return result
}

type astraIQTestHandler struct{}

func (astraIQTestHandler) Type() string            { return model.AstraIQTaskType }
func (astraIQTestHandler) Enabled() bool           { return model.AstraIQEnabled() }
func (astraIQTestHandler) Interval() time.Duration { return model.AstraIQInterval }
func (astraIQTestHandler) NewPayload() any         { return nil }
func (astraIQTestHandler) ScheduleFromStart() bool { return true }

func (astraIQTestHandler) ScheduleDue(now, lastRun int64) (bool, error) {
	settings, err := model.GetAstraIQSettings()
	return err == nil && settings.Due(now, lastRun), err
}

func (astraIQTestHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	settings, settingsErr := model.GetAstraIQSettings()
	if settingsErr != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, settingsErr)
		return
	}
	if !model.AstraIQEnabled() || !settings.Active(time.Now()) {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, nil, nil)
		return
	}
	userID, err := resolveChannelTestUserID(nil)
	if err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	var channels []*model.Channel
	if err = model.DB.Find(&channels).Error; err != nil {
		finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusFailed, nil, err)
		return
	}
	jobs := make(chan *model.Channel, len(channels))
	for _, ch := range channels {
		if model.ChannelEligibleForAstraIQ(ch) {
			jobs <- ch
		}
	}
	close(jobs)
	errs := make(chan error, len(channels))
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			for ch := range jobs {
				if ctx.Err() != nil {
					return
				}
				result := probeAstraIQChannel(ctx, ch, userID)
				if ctx.Err() != nil {
					return
				}
				if result.Status == "pending" {
					if result.Detail == "settings_unavailable" {
						errs <- errors.New("check settings unavailable")
					}
					continue
				}
				if err := model.SaveAstraIQResult(ch, result); err != nil {
					errs <- fmt.Errorf("save channel %d IQ result: %w", ch.Id, err)
				}
			}
		})
	}
	workers.Wait()
	close(errs)
	for saveErr := range errs {
		err = errors.Join(err, saveErr)
	}
	err = errors.Join(err, ctx.Err())
	status := model.SystemTaskStatusSucceeded
	if err != nil {
		status = model.SystemTaskStatusFailed
	}
	finishSystemTaskHandler(task, runnerID, status, nil, err)
}

// History details share the channel read permission; the response deliberately
// contains no channel configuration or raw upstream request/response envelope.
func GetAstraIQSample(c *gin.Context) {
	channelID, idErr := strconv.Atoi(c.Param("id"))
	checkedAt, timeErr := strconv.ParseInt(c.Param("checked_at"), 10, 64)
	if idErr != nil || timeErr != nil || channelID <= 0 || checkedAt <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid check identifier"})
		return
	}
	rows, err := model.GetAstraIQResults([]int{channelID})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	row := rows[channelID]
	var history []model.AstraIQSample
	if err := common.UnmarshalJsonStr(string(row.HistoryJSON), &history); err == nil && row.Version == model.AstraIQVersion {
		for _, sample := range history {
			if sample.CheckedAt != checkedAt {
				continue
			}
			// Older samples remain readable after upgrading the history format.
			if len(sample.Attempts) == 0 && checkedAt == row.CheckedAt {
				sample.Answer, sample.Detail, sample.LatencyMS = row.Answer, row.Detail, row.LatencyMS
			}
			common.ApiSuccess(c, gin.H{"sample": sample, "question": astraIQQuestion, "expected_answer": "21", "model": model.AstraIQModel, "reasoning_effort": "medium"})
			return
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Check history is no longer available"})
}

func DeleteAstraIQResults(c *gin.Context) {
	channelID, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid check identifier"})
		return
	}
	var checkedAt int64
	if value := c.Param("checked_at"); value != "" {
		checkedAt, err = strconv.ParseInt(value, 10, 64)
		if err != nil || checkedAt <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid check identifier"})
			return
		}
	}
	if err = model.DeleteAstraIQResults(channelID, checkedAt); errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Check history is no longer available"})
		return
	} else if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func GetAstraIQStatus(c *gin.Context) {
	views := make(map[int]model.AstraIQView)
	if !model.AstraIQEnabled() {
		common.ApiSuccess(c, gin.H{"enabled": false, "results": views})
		return
	}
	settings, err := model.GetAstraIQSettings()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !settings.Enabled {
		common.ApiSuccess(c, gin.H{"enabled": false, "results": views})
		return
	}
	var channels []*model.Channel
	if err := model.DB.Find(&channels).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	ids := make([]int, 0, len(channels))
	for _, ch := range channels {
		if model.ChannelSupportsAstraIQ(ch) {
			ids = append(ids, ch.Id)
		}
	}
	results, err := model.GetAstraIQResults(ids)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	for _, ch := range channels {
		if model.ChannelSupportsAstraIQ(ch) {
			views[ch.Id] = model.AstraIQResultView(ch, results[ch.Id], settings)
		}
	}
	common.ApiSuccess(c, gin.H{"enabled": true, "interval_minutes": settings.IntervalMinutes, "server_time": common.GetTimestamp(), "results": views})
}

func GetAstraIQSettings(c *gin.Context) {
	settings, err := model.GetAstraIQSettings()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, settings)
}

func UpdateAstraIQSettings(c *gin.Context) {
	var request struct {
		Enabled         *bool  `json:"enabled" binding:"required"`
		StartTime       string `json:"start_time" binding:"required"`
		EndTime         string `json:"end_time" binding:"required"`
		IntervalMinutes int    `json:"interval_minutes" binding:"required"`
		StopOnFailure   *bool  `json:"stop_on_failure" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid check settings"})
		return
	}
	settings := model.AstraIQSettings{
		Enabled:   *request.Enabled,
		StartTime: request.StartTime, EndTime: request.EndTime,
		IntervalMinutes: request.IntervalMinutes, StopOnFailure: *request.StopOnFailure,
	}
	if err := settings.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	if err := model.SaveAstraIQSettings(settings); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, settings)
}
