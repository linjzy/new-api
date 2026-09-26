package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	hostdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// The first shipped IQ table used MySQL TEXT for its compact history.
type astraIQLegacyRow struct {
	ChannelID   int    `gorm:"primaryKey;autoIncrement:false"`
	HistoryJSON string `gorm:"type:text"`
}

func (astraIQLegacyRow) TableName() string { return "astra_iq_results" }

func TestCustomAstraIQGradesOnlyCompletedAssistantAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		passed, invalid bool
	}{
		{"chat correct", `{"choices":[{"message":{"role":"assistant","content":"21"},"finish_reason":"stop"}]}`, true, false},
		{"wrong", `{"choices":[{"message":{"content":"22"},"finish_reason":"stop"}]}`, false, false},
		{"not substring", `{"choices":[{"message":{"content":"121"},"finish_reason":"stop"}]}`, false, false},
		{"reasoning cannot pass", `{"choices":[{"message":{"content":"22","reasoning_content":"21"},"finish_reason":"stop"}]}`, false, false},
		{"ambiguous answer", `{"choices":[{"message":{"content":"21或22"},"finish_reason":"stop"}]}`, false, false},
		{"truncated", `{"choices":[{"message":{"content":"21"},"finish_reason":"length"}]}`, false, true},
		{"refusal", `{"choices":[{"message":{"refusal":"21"},"finish_reason":"stop"}]}`, false, true},
		{"responses correct", `{"status":"completed","output":[{"type":"reasoning","content":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}]}`, true, false},
		{"responses unfinished", `{"status":"incomplete","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}]}`, false, true},
		{"split chat stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"2\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"1\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n", true, false},
		{"cut stream", "data: {\"choices\":[{\"delta\":{\"content\":\"21\"}}]}\n", false, true},
		{"error stream", "data: {\"error\":{\"message\":\"21\"}}\n", false, true},
		{"completed responses stream", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"error\":null,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n", true, false},
		{"responses error without error object", "event: error\ndata: {\"type\":\"error\",\"message\":\"21\"}\n\n", false, true},
		{"completed with final message event and empty output", "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n", true, false},
		{"final message without completed response", "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}}\n\n", false, true},
		{"commentary cannot substitute final answer", "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"commentary\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := astraIQAnswer([]byte(tc.body))
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.passed, astraIQAnswerPattern.MatchString(answer))
		})
	}
}

func TestCustomAstraIQForcesMediumAfterChannelOverrides(t *testing.T) {
	for _, tc := range []struct{ body, path string }{
		{`{"messages":[{"role":"user","content":"question"}],"reasoning_effort":"high"}`, "reasoning_effort"},
		{`{"input":"question","reasoning":{"effort":"low","summary":"auto"}}`, "reasoning.effort"},
	} {
		encoded, err := enforceAstraIQReasoning([]byte(tc.body))
		require.NoError(t, err)
		assert.Equal(t, "medium", gjson.GetBytes(encoded, tc.path).String())
	}
	response := &dto.OpenAIResponsesRequest{}
	require.NoError(t, setAstraIQPrompt(response, astraIQQuestion))
	require.NotNil(t, response.Reasoning)
	assert.Equal(t, "medium", response.Reasoning.Effort)
	assert.NotContains(t, string(response.Input), "21")
}

func TestCustomAstraIQDatabaseAndRouting(t *testing.T) {
	t.Setenv("ASTRA_IQ_ENABLED", "true")
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			dsn := os.Getenv(dialect.env)
			if dialect.env != "" && dsn == "" {
				t.Skip(dialect.env + " not configured")
			}
			db := modelManagementDB(t, dialect.kind, dsn)
			t.Run("atomic settings API and persistent reload", func(t *testing.T) {
				settings, err := model.GetAstraIQSettings()
				require.NoError(t, err)
				assert.Equal(t, model.DefaultAstraIQSettings(), settings)
				legacy := model.Option{Key: "AstraIQSettings", Value: `{"start_time":"00:00","end_time":"00:00","interval_minutes":1,"stop_on_failure":true}`}
				require.NoError(t, db.Create(&legacy).Error)
				settings, err = model.GetAstraIQSettings()
				require.NoError(t, err)
				assert.True(t, settings.Enabled, "existing saved schedules remain enabled")
				assert.Equal(t, 1, settings.IntervalMinutes)
				engine := gin.New()
				engine.GET("/settings", GetAstraIQSettings)
				engine.PUT("/settings", UpdateAstraIQSettings)
				valid := `{"enabled":true,"start_time":"22:00","end_time":"06:30","interval_minutes":17,"stop_on_failure":false}`
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(valid)))
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				for _, invalid := range []string{
					strings.Replace(valid, "22:00", "24:00", 1),
					strings.Replace(valid, "06:30", "6:30", 1),
					strings.Replace(valid, ":17,", ":0,", 1),
					strings.Replace(valid, ":17,", ":1441,", 1),
					strings.Replace(valid, ":17,", ":1.5,", 1),
					strings.Replace(valid, `,"stop_on_failure":false`, "", 1),
					`{}`, `null`,
				} {
					w = httptest.NewRecorder()
					engine.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(invalid)))
					assert.Equal(t, http.StatusBadRequest, w.Code, invalid)
				}
				// Read through another GORM session instead of relying on process state.
				previousDB := model.DB
				model.DB = db.Session(&gorm.Session{NewDB: true})
				defer func() { model.DB = previousDB }()
				w = httptest.NewRecorder()
				engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings", nil))
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.JSONEq(t, valid, gjson.GetBytes(w.Body.Bytes(), "data").Raw)
				require.NoError(t, model.SaveAstraIQSettings(model.DefaultAstraIQSettings()))
			})
			channels := []model.Channel{
				{Id: 201, Name: "high priority", Type: 1, Key: "test-a", Status: common.ChannelStatusEnabled, Models: model.AstraIQModel + ",unrelated-model", Group: "default", Priority: common.GetPointer(int64(10))},
				{Id: 202, Name: "fallback", Type: 1, Key: "test-b", Status: common.ChannelStatusEnabled, Models: model.AstraIQModel, Group: "default", Priority: common.GetPointer(int64(5))},
			}
			for i := range channels {
				require.NoError(t, db.Create(&channels[i]).Error)
				require.NoError(t, channels[i].AddAbilities(db))
			}
			// Upgrade representative pre-patch channel data. Repeated startup must
			// preserve approvals/history and leave existing channels intact.
			require.False(t, db.Migrator().HasTable(&model.AstraIQResult{}))
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.AstraIQResult{}))
			}
			require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "pass", CheckedAt: common.GetTimestamp(), Answer: "21"}))
			assert.True(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel))
			require.NoError(t, db.Migrator().DropTable(&model.AstraIQResult{}))
			require.NoError(t, db.AutoMigrate(&astraIQLegacyRow{}))
			require.NoError(t, db.Create(&astraIQLegacyRow{ChannelID: 909, HistoryJSON: `[{"status":"pass","checked_at":100}]`}).Error)
			require.NoError(t, db.AutoMigrate(&model.AstraIQResult{}))
			var preserved model.AstraIQResult
			require.NoError(t, db.First(&preserved, "channel_id = ?", 909).Error)
			assert.Equal(t, `[{"status":"pass","checked_at":100}]`, string(preserved.HistoryJSON))
			largeSample := model.AstraIQSample{Attempts: []model.AstraIQAttempt{{Response: strings.Repeat("检测回复", 20000)}}}
			require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "pass", CheckedAt: common.GetTimestamp(), Sample: largeSample}))
			largeRows, err := model.GetAstraIQResults([]int{channels[0].Id})
			require.NoError(t, err)
			assert.Greater(t, len(largeRows[channels[0].Id].HistoryJSON), 65535, "detailed history must fit beyond MySQL TEXT capacity")
			require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "pass", CheckedAt: common.GetTimestamp(), Answer: "21"}))
			for range 2 {
				require.NoError(t, db.AutoMigrate(&model.AstraIQResult{}))
			}
			var unchanged model.Channel
			require.NoError(t, db.First(&unchanged, channels[0].Id).Error)
			assert.Equal(t, "test-a", unchanged.Key)
			assert.True(t, model.AstraIQAllowsChannel(&unchanged, model.AstraIQModel))
			for _, cached := range []bool{false, true} {
				t.Run(fmt.Sprintf("cache=%t", cached), func(t *testing.T) {
					common.MemoryCacheEnabled = cached
					model.InitChannelCache()
					require.NoError(t, db.Where("channel_id IN ?", []int{201, 202}).Delete(&model.AstraIQResult{}).Error)
					selected, err := model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
					require.NoError(t, err)
					require.NotNil(t, selected)
					assert.Equal(t, 201, selected.Id, "untested channels must be allowed")
					require.NoError(t, model.SaveAstraIQResult(&channels[1], model.AstraIQResult{Status: "pass", CheckedAt: common.GetTimestamp()}))
					selected, err = model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
					require.NoError(t, err)
					require.NotNil(t, selected)
					assert.Equal(t, 201, selected.Id, "an untested higher-priority channel stays eligible")
					require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "pass", CheckedAt: common.GetTimestamp()}))
					selected, err = model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
					require.NoError(t, err)
					require.NotNil(t, selected)
					assert.Equal(t, 201, selected.Id)
					require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "fail", CheckedAt: common.GetTimestamp(), Answer: "22"}))
					selected, err = model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
					require.NoError(t, err)
					require.NotNil(t, selected)
					assert.Equal(t, 202, selected.Id, "failure must apply without refreshing channel cache")
					pinned := newPinRetryContext()
					service.GetChannelConstraints(pinned).AddPin(hostdto.ChannelPin{ChannelId: 201, Source: hostdto.PinSourceToken, Rank: hostdto.PinRankToken, RetryMode: hostdto.PinRetrySingleAttempt})
					_, _, pinErr := service.SelectChannelForRequest(pinned, model.AstraIQModel, &service.RetryParam{Ctx: pinned, ModelName: model.AstraIQModel, TokenGroup: "default", Retry: common.GetPointer(0)})
					require.NotNil(t, pinErr)
					assert.Equal(t, hostdto.ChannelFilterKind("astra_iq"), pinErr.FilterKind)
					selected, err = model.GetRandomSatisfiedChannel("default", "unrelated-model", 0, nil)
					require.NoError(t, err)
					require.NotNil(t, selected)
					assert.Equal(t, 201, selected.Id)
					require.NoError(t, model.SaveAstraIQResult(&channels[1], model.AstraIQResult{Status: "pass", CheckedAt: common.GetTimestamp() - 24*60*60}))
					selected, err = model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
					require.NoError(t, err)
					require.NotNil(t, selected)
					assert.Equal(t, 202, selected.Id, "retain the latest result between scheduled checks")
					settings := model.DefaultAstraIQSettings()
					settings.Enabled = false
					require.NoError(t, model.SaveAstraIQSettings(settings))
					selected, err = model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
					require.NoError(t, err)
					require.NotNil(t, selected)
					assert.Equal(t, 201, selected.Id, "disabling checks must immediately restore cached routing")
					assert.True(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel))
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					GetAstraIQStatus(c)
					require.Equal(t, http.StatusOK, w.Code)
					assert.False(t, gjson.GetBytes(w.Body.Bytes(), "data.enabled").Bool())
					assert.Equal(t, "{}", gjson.GetBytes(w.Body.Bytes(), "data.results").Raw)
					settings.Enabled, settings.StopOnFailure = true, false
					require.NoError(t, model.SaveAstraIQSettings(settings))
					assert.True(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel))
					require.NoError(t, model.SaveAstraIQSettings(model.DefaultAstraIQSettings()))
					require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "pass", CheckedAt: common.GetTimestamp()}))
					assert.True(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel), "next correct probe restores routing")
					changed := channels[0]
					changed.Key = "replacement-key"
					assert.True(t, model.AstraIQAllowsChannel(&changed, model.AstraIQModel), "changed credentials have no applicable result yet")
					rows, err := model.GetAstraIQResults([]int{201})
					require.NoError(t, err)
					view := model.AstraIQResultView(&channels[0], rows[201], model.DefaultAstraIQSettings())
					assert.InDelta(t, 66.6667, view.PassRate, 0.001)
					encoded, err := common.Marshal(view)
					require.NoError(t, err)
					assert.NotContains(t, string(encoded), "fingerprint")
					assert.NotContains(t, string(encoded), "test-a")
				})
			}
			t.Run("delete history without restoring stale approval", func(t *testing.T) {
				require.NoError(t, db.Where("channel_id = ?", 201).Delete(&model.AstraIQResult{}).Error)
				now := common.GetTimestamp()
				for _, result := range []model.AstraIQResult{
					{Status: "pass", CheckedAt: now - 30, Answer: "21"},
					{Status: "fail", CheckedAt: now - 20, Answer: "29"},
					{Status: "pass", CheckedAt: now - 10, Answer: "21"},
				} {
					require.NoError(t, model.SaveAstraIQResult(&channels[0], result))
				}
				before, err := model.GetAstraIQResults([]int{202})
				require.NoError(t, err)
				engine := gin.New()
				engine.DELETE("/checks/:id", DeleteAstraIQResults)
				engine.DELETE("/checks/:id/:checked_at", DeleteAstraIQResults)
				for _, tc := range []struct {
					path    string
					code    int
					count   int
					allowed bool
				}{
					{"/checks/bad", 400, 3, true},
					{"/checks/201/0", 400, 3, true},
					{"/checks/201/999", 404, 3, true},
					{fmt.Sprintf("/checks/201/%d", now-30), 200, 2, true},
					{fmt.Sprintf("/checks/201/%d", now-10), 200, 1, true},
					{"/checks/201", 200, 0, true},
					{"/checks/201", 200, 0, true},
				} {
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, tc.path, nil))
					require.Equal(t, tc.code, w.Code, w.Body.String())
					rows, err := model.GetAstraIQResults([]int{201, 202})
					require.NoError(t, err)
					view := model.AstraIQResultView(&channels[0], rows[201], model.DefaultAstraIQSettings())
					assert.Len(t, view.History, tc.count)
					assert.Equal(t, tc.allowed, view.Allowed)
					assert.Equal(t, before[202], rows[202], "deletion must be scoped to the selected channel")
					if tc.count <= 1 {
						assert.Equal(t, "pending", view.Status)
						assert.Zero(t, view.CheckedAt)
						assert.Empty(t, view.Answer)
					}
				}
				require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "pass", CheckedAt: now, Answer: "21"}))
				rows, err := model.GetAstraIQResults([]int{201})
				require.NoError(t, err)
				assert.Len(t, model.AstraIQResultView(&channels[0], rows[201], model.DefaultAstraIQSettings()).History, 1, "new checks must not resurrect deleted history")
				require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "fail", CheckedAt: now + 1, Answer: "29"}))
				require.NoError(t, model.DeleteAstraIQResults(201, now+1))
				assert.True(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel), "deleting the latest result permits untested calls")
				rows, err = model.GetAstraIQResults([]int{201})
				require.NoError(t, err)
				assert.Equal(t, "pending", rows[201].Status)
				assert.Zero(t, rows[201].CheckedAt, "an older result must not be promoted")
			})
		})
	}
}

func TestCustomAstraIQProbeUsesQuestionAndChecksEveryEnabledKey(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-6-astra":1}`))
	user := model.User{Id: 777321, Username: "iq-probe-fixture", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(&user).Error)
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request dto.OpenAIResponsesRequest
		if err := common.DecodeJson(r.Body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		assert.Equal(t, model.AstraIQModel, request.Model)
		assert.Equal(t, "/v1/responses", r.URL.Path)
		assert.Contains(t, r.UserAgent(), "codex", "the official client must originate the probe")
		assert.NotEmpty(t, r.Header.Get("originator"))
		require.NotNil(t, request.Reasoning)
		assert.Equal(t, "medium", request.Reasoning.Effort)
		require.NotNil(t, request.Stream)
		assert.True(t, *request.Stream)
		assert.Contains(t, gjson.GetBytes(request.Input, "@tostr").String(), "最少取出多少个糖果")
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer blocked" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"error":{"message":"This account only allows Codex official clients","type":"forbidden_error"}}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer fixture-http-secret" {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"message":"Upstream access forbidden. Authorization: Bearer fixture-http-secret","type":"upstream_error","code":"access_denied"},"private_metadata":"never-persist-this-body"}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer fixture-stream-secret" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"rate_limit_error\",\"code\":\"usage_limit_reached\",\"message\":\"The usage limit has been reached. api_key=fixture-stream-secret\"}}}\n\n")
			return
		}
		answer := "21"
		if r.Header.Get("Authorization") == "Bearer wrong" {
			answer = "22"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"test\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n")
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_test\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n", answer)
		fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":%q,\"annotations\":[]}]}}\n\n", answer)
		// The deployed upstream sometimes leaves output empty in the terminal
		// event after emitting the full assistant message in output_item.done.
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"test\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":1,\"total_tokens\":101}}}\n\n")
	}))
	defer upstream.Close()
	ch := &model.Channel{Id: 901, Type: constant.ChannelTypeOpenAI, Key: "correct\nwrong", BaseURL: &upstream.URL, Models: model.AstraIQModel, Group: "default", Status: common.ChannelStatusEnabled, ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2}}
	result := probeAstraIQChannel(context.Background(), ch, user.Id)
	assert.Equal(t, "fail", result.Status, result.Detail)
	assert.Equal(t, "22", result.Answer)
	require.Len(t, result.Sample.Attempts, 2)
	assert.Equal(t, "21", result.Sample.Attempts[0].Response)
	assert.Equal(t, "22", result.Sample.Attempts[1].Response)
	require.NotNil(t, result.Sample.Attempts[0].PromptTokens)
	assert.Equal(t, 100, *result.Sample.Attempts[0].PromptTokens)
	assert.NotNil(t, result.Sample.Attempts[0].FirstTokenMS)
	assert.Equal(t, []string{"Bearer correct", "Bearer wrong"}, seen)
	ch.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusAutoDisabled}
	result = probeAstraIQChannel(context.Background(), ch, user.Id)
	assert.Equal(t, "pass", result.Status, result.Detail)
	assert.Equal(t, 0, ch.ChannelInfo.MultiKeyPollingIndex)
	for _, status := range []int{common.ChannelStatusManuallyDisabled, common.ChannelStatusAutoDisabled} {
		ch.Status = status
		before := len(seen)
		assert.False(t, model.ChannelEligibleForAstraIQ(ch))
		result = probeAstraIQChannel(context.Background(), ch, user.Id)
		assert.Equal(t, "channel_disabled", result.Detail)
		assert.Len(t, seen, before, "disabled channels must not make upstream requests")
	}
	ch.Status, ch.Key, ch.ChannelInfo = common.ChannelStatusEnabled, "blocked", model.ChannelInfo{}
	result = probeAstraIQChannel(context.Background(), ch, user.Id)
	assert.Equal(t, "error", result.Status)
	assert.Equal(t, "official_client_rejected", result.Detail)
	require.Len(t, result.Sample.Attempts, 1)
	assert.Equal(t, http.StatusForbidden, result.Sample.Attempts[0].HTTPStatus)
	assert.Empty(t, result.Sample.Attempts[0].Response)
	assert.Equal(t, "This account only allows Codex official clients", result.Sample.Attempts[0].ErrorMessage)
	for _, tc := range []struct {
		key, code, message string
		httpStatus         int
	}{
		{"fixture-http-secret", "access_denied", "Upstream access forbidden", 502},
		{"fixture-stream-secret", "usage_limit_reached", "The usage limit has been reached", 200},
	} {
		ch.Key = tc.key
		result = probeAstraIQChannel(context.Background(), ch, user.Id)
		require.Equal(t, "error", result.Status)
		require.Len(t, result.Sample.Attempts, 1)
		attempt := result.Sample.Attempts[0]
		assert.Equal(t, tc.httpStatus, attempt.HTTPStatus)
		assert.Equal(t, tc.code, attempt.ErrorCode)
		assert.Contains(t, attempt.ErrorMessage, tc.message)
		encoded, err := common.Marshal(attempt)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), tc.key)
		assert.NotContains(t, string(encoded), "never-persist-this-body")
	}
	settings := model.DefaultAstraIQSettings()
	settings.Enabled = false
	require.NoError(t, model.SaveAstraIQSettings(settings))
	before := len(seen)
	assert.Equal(t, "pending", probeAstraIQChannel(context.Background(), ch, user.Id).Status)
	assert.Len(t, seen, before, "the total switch must prevent upstream calls")
	require.NoError(t, model.SaveAstraIQSettings(model.DefaultAstraIQSettings()))
	ch.Key = "blocked"

	// The local bridge never accepts an unauthenticated call or replays its
	// one-time credential, including after the upstream denies the first call.
	proxyResults := make(chan testResult, 1)
	proxy := astraIQProxyHandler(context.Background(), ch, user.Id, "one-time-test-key", proxyResults)
	for _, request := range []struct{ method, path, token string }{
		{http.MethodPost, "/v1/responses", ""},
		{http.MethodPost, "/v1/responses", "wrong-key"},
		{http.MethodGet, "/v1/responses", "one-time-test-key"},
		{http.MethodPost, "/v1/chat/completions", "one-time-test-key"},
	} {
		r := httptest.NewRequest(request.method, request.path, nil)
		r.Header.Set("Authorization", "Bearer "+request.token)
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Empty(t, proxyResults)
	}
	for _, expectedStatus := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[{"role":"user","content":"最少取出多少个糖果"}],"reasoning":{"effort":"medium"},"stream":true}`))
		r.Header.Set("Authorization", "Bearer one-time-test-key")
		r.Header.Set("User-Agent", "codex-test-boundary")
		r.Header.Set("originator", "test-boundary")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		assert.Equal(t, expectedStatus, w.Code)
	}
	assert.Len(t, proxyResults, 1)
}

func TestCustomAstraIQHistoryDetails(t *testing.T) {
	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(&model.AstraIQResult{}))
	ch := &model.Channel{Id: 908, Key: "private-key", Models: model.AstraIQModel}
	first := model.AstraIQResult{Status: "error", CheckedAt: 100, LatencyMS: 1234, Sample: model.AstraIQSample{Endpoint: "/v1/responses", Attempts: []model.AstraIQAttempt{{KeyIndex: 1, Status: "error", ErrorMessage: "The service is temporarily unavailable.", ErrorCode: "service_unavailable", ErrorType: "upstream_error", HTTPStatus: 200}}}}
	require.NoError(t, model.SaveAstraIQResult(ch, first))
	require.NoError(t, model.SaveAstraIQResult(ch, model.AstraIQResult{Status: "pass", CheckedAt: 200, Answer: "21"}))
	rows, err := model.GetAstraIQResults([]int{ch.Id})
	require.NoError(t, err)
	assert.Empty(t, model.AstraIQResultView(ch, rows[ch.Id], model.DefaultAstraIQSettings()).History[0].Attempts, "polling endpoint must not carry all historical responses")
	for _, tc := range []struct {
		at   string
		code int
	}{{"100", 200}, {"200", 200}, {"999", 404}, {"bad", 400}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "908"}, {Key: "checked_at", Value: tc.at}}
		GetAstraIQSample(c)
		assert.Equal(t, tc.code, w.Code)
		assert.NotContains(t, w.Body.String(), "private-key")
		assert.NotContains(t, w.Body.String(), "fingerprint")
		if tc.at == "100" {
			assert.Equal(t, "The service is temporarily unavailable.", gjson.GetBytes(w.Body.Bytes(), "data.sample.attempts.0.error_message").String())
			assert.Equal(t, "service_unavailable", gjson.GetBytes(w.Body.Bytes(), "data.sample.attempts.0.error_code").String())
			assert.Equal(t, astraIQQuestion, gjson.GetBytes(w.Body.Bytes(), "data.question").String())
			assert.Equal(t, "medium", gjson.GetBytes(w.Body.Bytes(), "data.reasoning_effort").String())
		}
	}
}

func TestCustomAstraIQDailySchedule(t *testing.T) {
	stamp := func(value string) int64 {
		parsed, err := time.Parse(time.RFC3339, value)
		require.NoError(t, err)
		return parsed.Unix()
	}
	for _, tc := range []struct {
		name, start, end, now, last string
		interval                    int
		active, due                 bool
	}{
		{"before start", "09:00", "18:00", "2026-09-26T08:59:59+08:00", "2026-09-25T09:00:00+08:00", 5, false, false},
		{"start inclusive", "09:00", "18:00", "2026-09-26T09:00:00+08:00", "2026-09-25T17:59:00+08:00", 1440, true, true},
		{"wait interval", "09:00", "18:00", "2026-09-26T09:16:59+08:00", "2026-09-26T09:00:00+08:00", 17, true, false},
		{"interval due in UTC", "09:00", "18:00", "2026-09-26T01:17:00Z", "2026-09-26T09:00:00+08:00", 17, true, true},
		{"end exclusive", "09:00", "18:00", "2026-09-26T18:00:00+08:00", "2026-09-26T09:00:00+08:00", 5, false, false},
		{"overnight before midnight", "22:00", "06:00", "2026-09-26T23:59:00+08:00", "2026-09-26T22:00:00+08:00", 5, true, true},
		{"overnight does not reset at midnight", "22:00", "06:00", "2026-09-27T00:01:00+08:00", "2026-09-26T23:59:00+08:00", 5, true, false},
		{"overnight end", "22:00", "06:00", "2026-09-27T06:00:00+08:00", "2026-09-26T22:00:00+08:00", 5, false, false},
		{"equal means all day", "09:00", "09:00", "2026-09-27T08:59:00+08:00", "2026-09-26T23:59:00+08:00", 5, true, true},
		{"all day does not reset at midnight", "00:00", "00:00", "2026-09-27T00:01:00+08:00", "2026-09-26T23:59:00+08:00", 5, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := model.AstraIQSettings{Enabled: true, StartTime: tc.start, EndTime: tc.end, IntervalMinutes: tc.interval, StopOnFailure: true}
			require.NoError(t, settings.Validate())
			assert.Equal(t, tc.active, settings.Active(time.Unix(stamp(tc.now), 0)))
			assert.Equal(t, tc.due, settings.Due(stamp(tc.now), stamp(tc.last)))
			assert.Equal(t, tc.active, settings.Due(stamp(tc.now), 0), "first check obeys the window")
			settings.Enabled = false
			assert.False(t, settings.Due(stamp(tc.now), 0))
			assert.False(t, settings.Active(time.Unix(stamp(tc.now), 0)))
		})
	}
}
