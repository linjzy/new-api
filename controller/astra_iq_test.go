package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	ChannelID   int `gorm:"primaryKey;autoIncrement:false"`
	Status      string
	CheckedAt   int64
	Version     string
	Fingerprint string
	Answer      string
	Detail      string
	LatencyMS   int64
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
		encoded, err := enforceAstraIQReasoning([]byte(tc.body), model.AstraIQModel)
		require.NoError(t, err)
		assert.Equal(t, "medium", gjson.GetBytes(encoded, tc.path).String())
	}
	for _, tc := range []struct{ model, path, value string }{
		{"gemini-2.5-pro", "generationConfig.thinkingConfig.thinkingBudget", "8192"},
		{"gemini-3-flash-preview", "generationConfig.thinkingConfig.thinkingLevel", "medium"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			body := []byte(`{"contents":[{"role":"user","parts":[{"text":"question"}]}],"generationConfig":{"temperature":0.5,"thinkingConfig":{"thinkingBudget":32768,"thinkingLevel":"high","includeThoughts":true}},"reasoning_effort":"high"}`)
			encoded, err := enforceAstraIQReasoning(body, tc.model)
			require.NoError(t, err)
			assert.Equal(t, tc.value, gjson.GetBytes(encoded, tc.path).String())
			assert.Equal(t, "question", gjson.GetBytes(encoded, "contents.0.parts.0.text").String())
			assert.Equal(t, 0.5, gjson.GetBytes(encoded, "generationConfig.temperature").Float())
			assert.True(t, gjson.GetBytes(encoded, "generationConfig.thinkingConfig.includeThoughts").Bool())
			assert.False(t, gjson.GetBytes(encoded, "reasoning_effort").Exists())
			if strings.Contains(tc.path, "thinkingBudget") {
				assert.False(t, gjson.GetBytes(encoded, "generationConfig.thinkingConfig.thinkingLevel").Exists())
			} else {
				assert.False(t, gjson.GetBytes(encoded, "generationConfig.thinkingConfig.thinkingBudget").Exists())
			}
		})
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
			require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.AstraIQModelRun{}))
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
				assert.Empty(t, settings.ChannelIDs, "existing saved schedules check every channel")
				engine := gin.New()
				engine.GET("/settings", GetAstraIQSettings)
				engine.PUT("/settings", UpdateAstraIQSettings)
				valid := `{"enabled":true,"start_time":"22:00","end_time":"06:30","interval_minutes":17,"stop_on_failure":false,"channel_ids":[]}`
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
					strings.Replace(valid, `,"channel_ids":[]`, "", 1),
					strings.Replace(valid, "[]", "null", 1),
					strings.Replace(valid, "[]", "[0]", 1),
					strings.Replace(valid, "[]", "[999]", 1),
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
				data := gjson.GetBytes(w.Body.Bytes(), "data")
				assert.Equal(t, "22:00", data.Get("start_time").String())
				assert.Equal(t, "22:00", data.Get(`model_settings.gpt-6-astra.start_time`).String())
				assert.Empty(t, data.Get("channels").Array())
				require.NoError(t, model.SaveAstraIQSettings(model.DefaultAstraIQSettings()))
			})
			t.Run("status exposes recent check rounds without internal task data", func(t *testing.T) {
				for i := range model.AstraIQHistorySize + 1 {
					task := model.SystemTask{TaskID: fmt.Sprintf("iq-round-%d", i), Type: model.AstraIQTaskType, Status: model.SystemTaskStatusSucceeded, CreatedAt: 100 + int64(i)*75, UpdatedAt: 110 + int64(i)*75, Payload: "internal-task-payload"}
					if i == model.AstraIQHistorySize {
						task.Status = model.SystemTaskStatusRunning
					}
					require.NoError(t, db.Create(&task).Error)
				}
				require.NoError(t, db.Create(&model.SystemTask{TaskID: "unrelated-round", Type: model.SystemTaskTypeChannelTest, Status: model.SystemTaskStatusSucceeded, CreatedAt: 2000, UpdatedAt: 2010}).Error)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				GetAstraIQStatus(c)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				runs := gjson.GetBytes(w.Body.Bytes(), "data.runs").Array()
				require.Len(t, runs, model.AstraIQHistorySize)
				assert.JSONEq(t, `{"started_at":175,"finished_at":185}`, runs[0].Raw)
				assert.JSONEq(t, `{"started_at":1900,"finished_at":0}`, runs[len(runs)-1].Raw)
				assert.NotContains(t, w.Body.String(), "internal-task-payload")
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
				require.NoError(t, model.MigrateAstraIQResults(db))
			}
			require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "pass", CheckedAt: common.GetTimestamp(), Answer: "21"}))
			assert.True(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel))
			require.NoError(t, db.Migrator().DropTable(&model.AstraIQResult{}))
			require.NoError(t, db.AutoMigrate(&astraIQLegacyRow{}))
			require.NoError(t, db.Create(&astraIQLegacyRow{ChannelID: 909, HistoryJSON: `[{"status":"pass","checked_at":100}]`}).Error)
			require.NoError(t, model.MigrateAstraIQResults(db))
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
				require.NoError(t, model.MigrateAstraIQResults(db))
			}
			var unchanged model.Channel
			require.NoError(t, db.First(&unchanged, channels[0].Id).Error)
			assert.Equal(t, "test-a", unchanged.Key)
			assert.True(t, model.AstraIQAllowsChannel(&unchanged, model.AstraIQModel))
			t.Run("request profile preserves old failure and history", func(t *testing.T) {
				now := common.GetTimestamp()
				legacy := model.AstraIQResult{ChannelID: 201, ModelName: model.AstraIQModel, ModelKey: model.AstraIQModelKey(model.AstraIQModel), Status: "fail", CheckedAt: now - 10, Version: "candy-21-medium-v1", Fingerprint: model.AstraIQFingerprint(&channels[0]), Answer: "22", HistoryJSON: model.LongText(fmt.Sprintf(`[{"status":"fail","checked_at":%d,"answer":"22","client":"Codex CLI 0.157.1"}]`, now-10))}
				require.NoError(t, db.Save(&legacy).Error)
				assert.False(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel), "a profile change must not temporarily allow a failed channel")
				require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "pass", CheckedAt: now, Answer: "21", Sample: model.AstraIQSample{Profile: model.AstraIQProfile, Client: model.AstraIQClient + " (" + model.AstraIQProfile + ")"}}))
				rows, err := model.GetAstraIQResults([]int{201})
				require.NoError(t, err)
				var history []model.AstraIQSample
				require.NoError(t, common.UnmarshalJsonStr(string(rows[201].HistoryJSON), &history))
				require.Len(t, history, 2)
				assert.Equal(t, "22", history[0].Answer)
				assert.Empty(t, history[0].Profile, "missing profile identifies the original request configuration")
				assert.Equal(t, model.AstraIQProfile, history[1].Profile)
				assert.True(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel))
				engine := gin.New()
				engine.GET("/checks/:id/:checked_at", GetAstraIQSample)
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/checks/201/%d", now-10), nil))
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Equal(t, "22", gjson.GetBytes(w.Body.Bytes(), "data.sample.answer").String())
			})
			t.Run("blocked IQ database read does not hold channel cache lock", func(t *testing.T) {
				common.MemoryCacheEnabled = true
				model.InitChannelCache()
				entered, release, selected := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:astra_block", func(tx *gorm.DB) {
					if tx.Statement.Table == "options" {
						select {
						case entered <- struct{}{}:
						case <-release:
							return
						}
						<-release
					}
				}))
				defer db.Callback().Query().Remove("test:astra_block")
				go func() {
					defer close(selected)
					_, _ = model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("IQ query did not begin")
				}
				synced := make(chan struct{})
				go func() { model.InitChannelCache(); close(synced) }()
				select {
				case <-synced:
				case <-time.After(5 * time.Second):
					unblock()
					<-selected
					<-synced
					t.Fatal("slow IQ storage blocked channel synchronization")
				}
				unblock()
				other, err := model.GetRandomSatisfiedChannel("default", "unrelated-model", 0, nil)
				require.NoError(t, err)
				require.NotNil(t, other)
				assert.Equal(t, 201, other.Id)
				<-selected
			})
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
			t.Run("selected channels limit checks and gating", func(t *testing.T) {
				require.NoError(t, db.Create(&model.Channel{Id: 203, Name: "other model", Type: 1, Key: "test-c", Status: common.ChannelStatusEnabled, Models: "unrelated-model", Group: "default"}).Error)
				engine := gin.New()
				engine.GET("/settings", GetAstraIQSettings)
				engine.PUT("/settings", UpdateAstraIQSettings)
				save := func(ids string) *httptest.ResponseRecorder {
					w := httptest.NewRecorder()
					body := `{"enabled":true,"start_time":"00:00","end_time":"00:00","interval_minutes":5,"stop_on_failure":true,"channel_ids":` + ids + `}`
					engine.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(body)))
					return w
				}
				assert.Equal(t, http.StatusBadRequest, save("[203]").Code, "channels without the Astra model cannot be selected")
				w := save("[202,202]")
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.JSONEq(t, "[202]", gjson.GetBytes(w.Body.Bytes(), "data.channel_ids").Raw)
				w = httptest.NewRecorder()
				engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings", nil))
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				options := gjson.GetBytes(w.Body.Bytes(), "data.channels").Array()
				assert.Len(t, options, 3)
				assert.Equal(t, int64(201), options[0].Get("id").Int())
				assert.Contains(t, options[0].Get("models").Raw, model.AstraIQModel)
				assert.Equal(t, int64(202), options[1].Get("id").Int())
				assert.NotContains(t, w.Body.String(), "test-a")

				// An unselected channel's earlier failure neither blocks nor shows.
				require.NoError(t, model.SaveAstraIQResult(&channels[0], model.AstraIQResult{Status: "fail", CheckedAt: common.GetTimestamp(), Answer: "22"}))
				assert.True(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel))
				selected, err := model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
				require.NoError(t, err)
				require.NotNil(t, selected)
				assert.Equal(t, 201, selected.Id)
				w = httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				GetAstraIQStatus(c)
				require.Equal(t, http.StatusOK, w.Code)
				assert.False(t, gjson.GetBytes(w.Body.Bytes(), "data.results.201").Exists())
				assert.True(t, gjson.GetBytes(w.Body.Bytes(), "data.results.202").Exists())

				// Selecting it again applies that failure immediately.
				require.Equal(t, http.StatusOK, save("[201]").Code)
				assert.False(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel))
				selected, err = model.GetRandomSatisfiedChannel("default", model.AstraIQModel, 0, nil)
				require.NoError(t, err)
				require.NotNil(t, selected)
				assert.Equal(t, 202, selected.Id)
				require.Equal(t, http.StatusOK, save("[]").Code)
				assert.False(t, model.AstraIQAllowsChannel(&channels[0], model.AstraIQModel), "an empty selection checks every channel")
				require.NoError(t, model.SaveAstraIQSettings(model.DefaultAstraIQSettings()))
			})
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
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-6-astra":1,"gpt-6-sol":1}`))
	user := model.User{Id: 777321, Username: "iq-probe-fixture", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(&user).Error)
	var seen []string
	expectedModel := model.AstraIQModel
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request dto.OpenAIResponsesRequest
		if err := common.DecodeJson(r.Body, &request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		assert.Equal(t, expectedModel, request.Model)
		assert.Equal(t, "/v1/responses", r.URL.Path)
		assert.Contains(t, r.UserAgent(), "codex", "the official client must originate the probe")
		assert.NotEmpty(t, r.Header.Get("originator"))
		require.NotNil(t, request.Reasoning)
		assert.Equal(t, "medium", request.Reasoning.Effort)
		require.NotNil(t, request.Stream)
		assert.True(t, *request.Stream)
		assert.Contains(t, gjson.GetBytes(request.Input, "@tostr").String(), "最少取出多少个糖果")
		assert.NotContains(t, string(request.Input), "<multi_agent_role>")
		assert.NotContains(t, string(request.Input), "<multi_agent_mode>")
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
	assert.Equal(t, model.AstraIQProfile, result.Sample.Profile)
	assert.Contains(t, result.Sample.Client, model.AstraIQProfile)
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
	settings.Enabled, settings.ChannelIDs = true, []int{ch.Id + 1}
	require.NoError(t, model.SaveAstraIQSettings(settings))
	assert.Equal(t, "pending", probeAstraIQChannel(context.Background(), ch, user.Id).Status)
	assert.Len(t, seen, before, "a deselected channel must not start another check")
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

	// A scheduled round checks only the selected channels.
	t.Setenv("ASTRA_IQ_ENABLED", "true")
	require.NoError(t, db.AutoMigrate(&model.AstraIQResult{}, &model.AstraIQModelRun{}, &model.SystemTask{}, &model.SystemTaskLock{}))
	for _, scoped := range []model.Channel{
		{Id: 911, Name: "unselected", Type: constant.ChannelTypeOpenAI, Key: "scope-unselected", BaseURL: &upstream.URL, Models: model.AstraIQModel, Group: "default", Status: common.ChannelStatusEnabled},
		{Id: 912, Name: "selected", Type: constant.ChannelTypeOpenAI, Key: "scope-selected", BaseURL: &upstream.URL, Models: model.AstraIQModel, Group: "default", Status: common.ChannelStatusEnabled},
	} {
		require.NoError(t, db.Create(&scoped).Error)
	}
	settings.ChannelIDs = []int{912}
	require.NoError(t, model.SaveAstraIQSettings(settings))
	task, err := model.CreateSystemTask(model.AstraIQTaskType, nil, nil)
	require.NoError(t, err)
	_, claimed, err := model.ClaimSystemTask(task.ID, task.Type, "scope-runner", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, claimed)
	before = len(seen)
	astraIQTestHandler{}.Run(context.Background(), task, "scope-runner")
	assert.Equal(t, []string{"Bearer scope-selected"}, seen[before:])
	rows, err := model.GetAstraIQResults([]int{911, 912})
	require.NoError(t, err)
	assert.NotContains(t, rows, 911)
	assert.Equal(t, "pass", rows[912].Status)
	finished, err := model.GetLatestSystemTask(model.AstraIQTaskType)
	require.NoError(t, err)
	assert.Equal(t, model.SystemTaskStatusSucceeded, finished.Status)
	// The same official CLI and leased scheduler must really issue the newly
	// selected model, while the old model is independently paused.
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 912).Update("models", model.AstraIQModel+",gpt-6-sol").Error)
	astra := settings
	astra.Enabled = false
	sol := model.DefaultAstraIQSettings()
	sol.ChannelIDs = []int{912}
	sol.IntervalMinutes = 11
	require.NoError(t, model.SaveAstraIQConfig(model.AstraIQConfig{Models: map[string]model.AstraIQSettings{model.AstraIQModel: astra, "gpt-6-sol": sol}}))
	expectedModel = "gpt-6-sol"
	task, err = model.CreateSystemTask(model.AstraIQTaskType, nil, nil)
	require.NoError(t, err)
	_, claimed, err = model.ClaimSystemTask(task.ID, task.Type, "model-runner", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, claimed)
	before = len(seen)
	astraIQTestHandler{}.Run(context.Background(), task, "model-runner")
	assert.Equal(t, []string{"Bearer scope-selected"}, seen[before:])
	rows, err = model.GetAstraIQResults([]int{912}, "gpt-6-sol")
	require.NoError(t, err)
	assert.Equal(t, "pass", rows[912].Status)
	modelRuns, err := model.GetAstraIQModelRuns("gpt-6-sol")
	require.NoError(t, err)
	assert.Len(t, modelRuns, 1)

	// A local proxy token is scoped to the configured model as well as channel.
	probeResults := make(chan testResult, 1)
	scopedProxy := astraIQProxyHandler(context.Background(), ch, user.Id, "scoped-model-key", probeResults, "gpt-6-sol")
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[],"stream":true}`))
	r.Header.Set("Authorization", "Bearer scoped-model-key")
	w := httptest.NewRecorder()
	before = len(seen)
	scopedProxy.ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Len(t, seen, before)
	assert.ErrorContains(t, (<-probeResults).localErr, "probe model does not match configured model")
	require.NoError(t, model.SaveAstraIQConfig(model.AstraIQConfig{Models: map[string]model.AstraIQSettings{model.AstraIQModel: model.DefaultAstraIQSettings()}}))
}

func TestCustomAstraIQHistoryDetails(t *testing.T) {
	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, model.MigrateAstraIQResults(db))
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
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
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

func TestCustomAstraIQModelIsolationAndLegacyMigration(t *testing.T) {
	t.Setenv("ASTRA_IQ_ENABLED", "true")
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			dsn := os.Getenv(dialect.env)
			if dialect.env != "" && dsn == "" {
				t.Skip(dialect.env + " not configured")
			}
			db := modelManagementDB(t, dialect.kind, dsn)
			require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &astraIQLegacyRow{}))
			ch := model.Channel{Id: 971, Name: "two models", Type: constant.ChannelTypeOpenAI, Key: "isolated-key", Status: common.ChannelStatusEnabled, Models: model.AstraIQModel + ",gpt-6-sol,unrelated-model", Group: "default"}
			require.NoError(t, db.Create(&ch).Error)
			require.NoError(t, ch.AddAbilities(nil))
			// Upgrade preserves both the latest judgment and history, without changing
			// the old table used by a rollback image.
			legacy := model.AstraIQResult{ChannelID: ch.Id, Status: "fail", CheckedAt: 100, Version: model.AstraIQVersion, Fingerprint: model.AstraIQFingerprint(&ch), Answer: "22", HistoryJSON: `[{"status":"fail","checked_at":100,"answer":"22"}]`}
			require.NoError(t, db.Table("astra_iq_results").Omit("ModelName", "ModelKey").Create(&legacy).Error)
			// Exercise real application startup migrations twice on the legacy
			// database, keeping the fixture's original connection alive.
			common.IsMasterNode = true
			for range 2 {
				previousDB := model.DB
				require.NoError(t, model.InitDB())
				connection, connectionErr := model.DB.DB()
				require.NoError(t, connectionErr)
				require.NoError(t, connection.Close())
				model.DB = previousDB
			}
			common.IsMasterNode = false
			old, err := model.GetAstraIQResults([]int{ch.Id})
			require.NoError(t, err)
			assert.Equal(t, "fail", old[ch.Id].Status)
			assert.Equal(t, legacy.HistoryJSON, old[ch.Id].HistoryJSON)
			second := model.DefaultAstraIQSettings()
			second.IntervalMinutes = 10
			config := model.AstraIQConfig{Models: map[string]model.AstraIQSettings{model.AstraIQModel: model.DefaultAstraIQSettings(), "gpt-6-sol": second}}
			engine := gin.New()
			engine.PUT("/settings", UpdateAstraIQSettings)
			engine.GET("/settings", GetAstraIQSettings)
			engine.GET("/checks/:id/:checked_at", GetAstraIQSample)
			engine.DELETE("/checks/:id/:checked_at", DeleteAstraIQResults)
			body, err := common.Marshal(config)
			require.NoError(t, err)
			w := httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(string(body))))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			restored, err := model.GetAstraIQConfig()
			require.NoError(t, err)
			assert.Equal(t, config, restored)
			// The same channel can fail one model and pass another. Exercise both DB
			// and cache routing paths, including unrelated requests.
			require.NoError(t, model.SaveAstraIQResult(&ch, model.AstraIQResult{ModelName: "gpt-6-sol", Status: "pass", CheckedAt: 100, Answer: "21"}))
			for _, cached := range []bool{false, true} {
				common.MemoryCacheEnabled = cached
				model.InitChannelCache()
				for _, tc := range []struct {
					name    string
					allowed bool
				}{{model.AstraIQModel, false}, {"gpt-6-sol", true}, {"unrelated-model", true}} {
					assert.Equal(t, tc.allowed, model.AstraIQAllowsChannel(&ch, tc.name), tc.name)
					routed, routeErr := model.GetRandomSatisfiedChannel("default", tc.name, 0, nil)
					require.NoError(t, routeErr)
					assert.Equal(t, tc.allowed, routed != nil, tc.name)
				}
			}
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/checks/971/100?model=gpt-6-sol", nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "gpt-6-sol", gjson.GetBytes(w.Body.Bytes(), "data.model").String())
			assert.Equal(t, "21", gjson.GetBytes(w.Body.Bytes(), "data.sample.answer").String())
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/checks/971/100?model=gpt-6-sol", nil))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.False(t, model.AstraIQAllowsChannel(&ch, model.AstraIQModel))
			secondRows, err := model.GetAstraIQResults([]int{ch.Id}, "gpt-6-sol")
			require.NoError(t, err)
			assert.Zero(t, secondRows[ch.Id].CheckedAt)
			// Repeat migration after clearing Astra must never resurrect its failure.
			require.NoError(t, model.DeleteAstraIQResults(ch.Id, 0))
			require.NoError(t, model.MigrateAstraIQResults(db))
			assert.True(t, model.AstraIQAllowsChannel(&ch, model.AstraIQModel))
			var legacyUnchanged model.AstraIQResult
			require.NoError(t, db.Table("astra_iq_results").First(&legacyUnchanged, "channel_id = ?", ch.Id).Error)
			assert.Equal(t, "fail", legacyUnchanged.Status)
			// Independent start times drive distinct intervals, even when the global
			// system-task runner handles both models within the same leased round.
			require.NoError(t, db.Create(&model.AstraIQModelRun{ModelName: model.AstraIQModel, StartedAt: 1000, FinishedAt: 1020}).Error)
			require.NoError(t, db.Create(&model.AstraIQModelRun{ModelName: "gpt-6-sol", StartedAt: 1000, FinishedAt: 1030}).Error)
			due, err := model.DueAstraIQModels(1300, 0)
			require.NoError(t, err)
			assert.Contains(t, due, model.AstraIQModel)
			assert.NotContains(t, due, "gpt-6-sol")
			due, err = model.DueAstraIQModels(1600, 0)
			require.NoError(t, err)
			assert.Len(t, due, 2)
			// MySQL's default collation must not merge differently cased model
			// names that the channel exposes as distinct API identities.
			ch.Models += ",GPT-6-SOL"
			require.NoError(t, db.Model(&ch).Update("models", ch.Models).Error)
			upper := second
			config.Models["GPT-6-SOL"] = upper
			require.NoError(t, model.SaveAstraIQConfig(config))
			require.NoError(t, model.SaveAstraIQResult(&ch, model.AstraIQResult{ModelName: "GPT-6-SOL", Status: "fail", CheckedAt: 300, Answer: "29"}))
			assert.False(t, model.AstraIQAllowsChannel(&ch, "GPT-6-SOL"))
			assert.True(t, model.AstraIQAllowsChannel(&ch, "gpt-6-sol"))
			uppercase, readErr := model.GetAstraIQResults([]int{ch.Id}, "GPT-6-SOL")
			require.NoError(t, readErr)
			assert.Equal(t, "29", uppercase[ch.Id].Answer)
			delete(config.Models, "GPT-6-SOL")
			require.NoError(t, model.SaveAstraIQConfig(config))
			// Invalid model/channel pairs are rejected atomically.
			invalid := model.AstraIQConfig{Models: map[string]model.AstraIQSettings{"not-on-a-channel": second}}
			body, err = common.Marshal(invalid)
			require.NoError(t, err)
			w = httptest.NewRecorder()
			engine.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(string(body))))
			assert.Equal(t, http.StatusBadRequest, w.Code)
			restored, err = model.GetAstraIQConfig()
			require.NoError(t, err)
			assert.Equal(t, config, restored)
			require.NoError(t, model.SaveAstraIQResult(&ch, model.AstraIQResult{ModelName: "gpt-6-sol", Status: "fail", CheckedAt: 200, Answer: "29"}))
			delete(config.Models, "gpt-6-sol")
			require.NoError(t, model.SaveAstraIQConfig(config))
			assert.True(t, model.AstraIQAllowsChannel(&ch, "gpt-6-sol"))
			config.Models["gpt-6-sol"] = second
			require.NoError(t, model.SaveAstraIQConfig(config))
			assert.False(t, model.AstraIQAllowsChannel(&ch, "gpt-6-sol"))
		})
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

// This configured native Gemini channel can answer the benchmark. A local
// protocol conversion must not turn a healthy channel into a failed IQ gate.
func TestCustomAstraIQNativeGeminiCheckKeepsHealthyModelAvailable(t *testing.T) {
	t.Setenv("ASTRA_IQ_ENABLED", "true")
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	require.NoError(t, model.MigrateAstraIQResults(db))
	const name = "gemini-2.5-pro"
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gemini-2.5-pro":1}`))
	user := model.User{Id: 778812, Username: "gemini-iq-review", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default"}
	require.NoError(t, db.Create(&user).Error)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"21"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1,"totalTokenCount":11}}`+"\n\n")
	}))
	defer upstream.Close()
	ch := model.Channel{Id: 778, Name: "native Gemini fixture", Type: constant.ChannelTypeGemini, Key: "fixture-only-key", BaseURL: &upstream.URL, Models: name, Group: "default", Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(&ch).Error)
	engine := gin.New()
	engine.PUT("/settings", UpdateAstraIQSettings)
	saved := httptest.NewRecorder()
	engine.ServeHTTP(saved, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(`{"model_settings":{"gemini-2.5-pro":{"enabled":true,"start_time":"00:00","end_time":"00:00","interval_minutes":5,"stop_on_failure":true,"channel_ids":[]}}}`)))
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	var savedResponse struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(saved.Body.Bytes(), &savedResponse))
	require.True(t, savedResponse.Success, saved.Body.String())
	settings, err := model.GetAstraIQSettings(name)
	require.NoError(t, err)
	require.True(t, settings.Enabled)
	require.True(t, settings.StopOnFailure)
	t.Logf("API accepted native Gemini model: enabled=%t stop_on_failure=%t", settings.Enabled, settings.StopOnFailure)
	require.True(t, model.AstraIQAllowsChannel(&ch, name))
	result := probeAstraIQChannel(context.Background(), &ch, user.Id, name)
	t.Logf("status=%s detail=%s upstream_calls=%d attempts=%+v", result.Status, result.Detail, calls.Load(), result.Sample.Attempts)
	require.NoError(t, model.SaveAstraIQResult(&ch, result))
	assert.Equal(t, "pass", result.Status, "the configured native Gemini check should complete")
	assert.EqualValues(t, 1, calls.Load(), "one benchmark request should reach the healthy upstream")
	assert.True(t, model.AstraIQAllowsChannel(&ch, name), "a local conversion failure must not block all normal Gemini requests")
}
