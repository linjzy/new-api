package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	AstraIQModel   = "gpt-6-astra"
	AstraIQClient  = "Codex CLI 0.157.1"
	AstraIQVersion = "candy-21-medium-v1"
	// The question and routing judgment remain compatible across request
	// profiles. Keep the benchmark version/fingerprint so existing failures
	// stay blocked until a new check completes, and old history is retained.
	AstraIQProfile     = "minimal-v1"
	AstraIQTaskType    = "astra_iq_test"
	AstraIQInterval    = 5 * time.Minute
	AstraIQHistorySize = 24
)

// Separate storage prevents channel edits/imports from overwriting approval.
type AstraIQResult struct {
	ChannelID   int           `json:"channel_id" gorm:"primaryKey;autoIncrement:false"`
	ModelKey    string        `json:"-" gorm:"primaryKey;type:varchar(64)"`
	ModelName   string        `json:"model" gorm:"type:varchar(128)"`
	Status      string        `json:"status" gorm:"type:varchar(16)"`
	CheckedAt   int64         `json:"checked_at" gorm:"bigint"`
	Version     string        `json:"version" gorm:"type:varchar(64)"`
	Fingerprint string        `json:"-" gorm:"type:varchar(64)"`
	Answer      string        `json:"answer" gorm:"type:text"`
	Detail      string        `json:"detail" gorm:"type:varchar(128)"`
	LatencyMS   int64         `json:"latency_ms" gorm:"bigint"`
	HistoryJSON LongText      `json:"-"`
	Sample      AstraIQSample `json:"-" gorm:"-"`
}

type AstraIQSample struct {
	Status    string           `json:"status"`
	CheckedAt int64            `json:"checked_at"`
	StartedAt int64            `json:"started_at,omitempty"`
	Answer    string           `json:"answer,omitempty"`
	Detail    string           `json:"detail,omitempty"`
	LatencyMS int64            `json:"latency_ms,omitempty"`
	Endpoint  string           `json:"endpoint,omitempty"`
	Client    string           `json:"client,omitempty"`
	Profile   string           `json:"profile,omitempty"`
	Attempts  []AstraIQAttempt `json:"attempts,omitempty"`
}

// Store the assistant's answer, sanitized errors and safe metrics, never request
// headers, credentials, reasoning content or raw upstream error bodies.
type AstraIQAttempt struct {
	KeyIndex         int    `json:"key_index"`
	Status           string `json:"status"`
	Response         string `json:"response,omitempty"`
	Detail           string `json:"detail,omitempty"`
	ErrorMessage     string `json:"error_message,omitempty"`
	ErrorCode        string `json:"error_code,omitempty"`
	ErrorType        string `json:"error_type,omitempty"`
	LatencyMS        int64  `json:"latency_ms"`
	FirstTokenMS     *int64 `json:"first_token_ms"`
	PromptTokens     *int   `json:"prompt_tokens"`
	CompletionTokens *int   `json:"completion_tokens"`
	HTTPStatus       int    `json:"http_status,omitempty"`
}

type AstraIQView struct {
	AstraIQResult
	Allowed  bool            `json:"allowed"`
	PassRate float64         `json:"pass_rate"`
	History  []AstraIQSample `json:"history"`
}

type AstraIQRun struct {
	StartedAt  int64 `json:"started_at"`
	FinishedAt int64 `json:"finished_at"`
}

// Actual rounds distinguish scheduler drift and long-running probes from
// missed checks. Only their times are exposed, never task payloads or errors.
func GetAstraIQRuns() ([]AstraIQRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var tasks []SystemTask
	if err := DB.WithContext(ctx).Select("created_at", "updated_at", "status").
		Where("type = ?", AstraIQTaskType).Order("id DESC").Limit(AstraIQHistorySize).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	runs := make([]AstraIQRun, 0, len(tasks))
	for i := len(tasks) - 1; i >= 0; i-- {
		run := AstraIQRun{StartedAt: tasks[i].CreatedAt}
		if tasks[i].Status == SystemTaskStatusSucceeded || tasks[i].Status == SystemTaskStatusFailed {
			run.FinishedAt = tasks[i].UpdatedAt
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func AstraIQEnabled() bool {
	return common.GetEnvOrDefaultBool("ASTRA_IQ_ENABLED", false)
}

func AstraIQRequired(modelName string) bool {
	return AstraIQEnabled() && strings.TrimSpace(modelName) != ""
}

func ChannelSupportsAstraIQ(ch *Channel, names ...string) bool {
	if ch == nil {
		return false
	}
	for name := range strings.SplitSeq(ch.Models, ",") {
		if strings.TrimSpace(name) == AstraIQModelName(names...) {
			return true
		}
	}
	return false
}

func ChannelEligibleForAstraIQ(ch *Channel, names ...string) bool {
	return ch != nil && ch.Status == common.ChannelStatusEnabled && ChannelSupportsAstraIQ(ch, names...)
}

// Never return this hash in the API. Credential/configuration changes require
// another probe, including changes to the enabled multi-key set.
func AstraIQFingerprint(ch *Channel) string {
	data, err := common.Marshal([]any{AstraIQClient, ch.Type, ch.Key, ch.BaseURL, ch.OpenAIOrganization, ch.ModelMapping, ch.ParamOverride, ch.HeaderOverride, ch.Setting, ch.OtherSettings, ch.Other, ch.ChannelInfo.MultiKeyStatusList})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (r AstraIQResult) applicable(ch *Channel) bool {
	return ch != nil && r.CheckedAt > 0 && r.Version == AstraIQVersion &&
		r.Fingerprint != "" && r.Fingerprint == AstraIQFingerprint(ch)
}

func (r AstraIQResult) Allows(ch *Channel, settings AstraIQSettings) bool {
	if ch == nil {
		return false
	}
	// No result (including cleared history or changed credentials) is allowed.
	// Only an actual failure blocks calls, and it remains in force outside the
	// check window until a new pass, deletion, or disabling this setting.
	return !settings.Enabled || !settings.StopOnFailure || !settings.Includes(ch.Id) || !r.applicable(ch) || (r.Status != "fail" && r.Status != "error")
}

// Read shared storage at selection time so a failed probe takes effect on every
// instance immediately, independently of the ordinary channel cache refresh.
func GetAstraIQResults(ids []int, names ...string) (map[int]AstraIQResult, error) {
	results := make(map[int]AstraIQResult)
	if len(ids) == 0 {
		return results, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var rows []AstraIQResult
	err := DB.WithContext(ctx).Where("channel_id IN ? AND model_key = ?", ids, AstraIQModelKey(AstraIQModelName(names...))).Find(&rows).Error
	for _, row := range rows {
		results[row.ChannelID] = row
	}
	return results, err
}

func AstraIQAllowsChannel(ch *Channel, modelName string) bool {
	if !AstraIQRequired(modelName) {
		return true
	}
	if ch == nil {
		return false
	}
	settings, err := GetAstraIQSettings(modelName)
	if err != nil {
		return false
	}
	if !settings.Enabled || !settings.StopOnFailure || !settings.Includes(ch.Id) {
		return true
	}
	results, err := GetAstraIQResults([]int{ch.Id}, modelName)
	return err == nil && results[ch.Id].Allows(ch, settings)
}

type astraIQRoutingSnapshot struct {
	settings AstraIQSettings
	results  map[int]AstraIQResult
}

// Read once per selection, outside channelSyncLock. Load only judgment fields:
// full answers/history are irrelevant to routing and can be much larger.
func loadAstraIQRouting(modelName string) (*astraIQRoutingSnapshot, error) {
	if !AstraIQRequired(modelName) {
		return nil, nil
	}
	settings, err := GetAstraIQSettings(modelName)
	if err != nil {
		return nil, err
	}
	if !settings.Enabled || !settings.StopOnFailure {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var rows []AstraIQResult
	query := DB.WithContext(ctx).Select("channel_id", "status", "checked_at", "version", "fingerprint").Where("model_key = ?", AstraIQModelKey(modelName))
	if len(settings.ChannelIDs) > 0 {
		query = query.Where("channel_id IN ?", settings.ChannelIDs)
	}
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	snapshot := &astraIQRoutingSnapshot{settings: settings, results: make(map[int]AstraIQResult, len(rows))}
	for _, row := range rows {
		snapshot.results[row.ChannelID] = row
	}
	return snapshot, nil
}

// Caller holds channelSyncLock. Check the current cached channel fingerprint,
// including any credentials/configuration changed while the DB was queried.
func (s *astraIQRoutingSnapshot) filterIDs(ids []int) []int {
	if s == nil {
		return ids
	}
	kept := make([]int, 0, len(ids))
	for _, id := range ids {
		if s.results[id].Allows(channelsIDM[id], s.settings) {
			kept = append(kept, id)
		}
	}
	return kept
}

func SaveAstraIQResult(ch *Channel, result AstraIQResult) error {
	result.ModelName = AstraIQModelName(result.ModelName)
	result.ModelKey = AstraIQModelKey(result.ModelName)
	return DB.Transaction(func(tx *gorm.DB) error {
		// Serialize history changes with deletion so a finishing probe cannot
		// write back a copy of history that an administrator just removed.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&AstraIQResult{ChannelID: ch.Id, ModelName: result.ModelName, ModelKey: result.ModelKey}).Error; err != nil {
			return err
		}
		var previous AstraIQResult
		if err := lockForUpdate(tx).First(&previous, "channel_id = ? AND model_key = ?", ch.Id, AstraIQModelKey(result.ModelName)).Error; err != nil {
			return err
		}
		var history []AstraIQSample
		if previous.Version == AstraIQVersion {
			_ = common.UnmarshalJsonStr(string(previous.HistoryJSON), &history)
		}
		sample := result.Sample
		sample.Status, sample.CheckedAt = result.Status, result.CheckedAt
		sample.Answer, sample.Detail, sample.LatencyMS = result.Answer, result.Detail, result.LatencyMS
		history = append(history, sample)
		if len(history) > AstraIQHistorySize {
			history = history[len(history)-AstraIQHistorySize:]
		}
		data, err := common.Marshal(history)
		if err != nil {
			return err
		}
		result.ChannelID = ch.Id
		result.Version = AstraIQVersion
		result.Fingerprint = AstraIQFingerprint(ch)
		result.HistoryJSON = LongText(data)
		return tx.Save(&result).Error
	})
}

// checkedAt == 0 clears this channel's entire history. Removing the latest
// result resets the channel to untested; an older sample is never promoted.
func DeleteAstraIQResults(channelID int, checkedAt int64, names ...string) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var result AstraIQResult
		if err := lockForUpdate(tx).First(&result, "channel_id = ? AND model_key = ?", channelID, AstraIQModelKey(AstraIQModelName(names...))).Error; err != nil {
			if checkedAt == 0 && errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		history := []AstraIQSample{}
		if checkedAt != 0 {
			if err := common.UnmarshalJsonStr(string(result.HistoryJSON), &history); err != nil {
				return err
			}
			count := len(history)
			history = slices.DeleteFunc(history, func(sample AstraIQSample) bool { return sample.CheckedAt == checkedAt })
			if len(history) == count {
				return gorm.ErrRecordNotFound
			}
		}
		if checkedAt == 0 || checkedAt == result.CheckedAt {
			result.Status, result.CheckedAt = "pending", 0
			result.Answer, result.Detail, result.Fingerprint = "", "", ""
			result.LatencyMS = 0
		}
		data, err := common.Marshal(history)
		if err != nil {
			return err
		}
		result.HistoryJSON = LongText(data)
		return tx.Save(&result).Error
	})
}

func AstraIQResultView(ch *Channel, r AstraIQResult, settings AstraIQSettings) AstraIQView {
	view := AstraIQView{AstraIQResult: r, History: []AstraIQSample{}}
	view.ChannelID = ch.Id
	view.Allowed = r.Allows(ch, settings)
	if view.Status == "" || !r.applicable(ch) {
		view.Status = "pending"
	}
	_ = common.UnmarshalJsonStr(string(r.HistoryJSON), &view.History)
	passed := 0
	for _, sample := range view.History {
		if sample.Status == "pass" {
			passed++
		}
	}
	// Full replies are fetched only when a history segment is opened.
	for i := range view.History {
		view.History[i].Attempts = nil
	}
	if len(view.History) > 0 {
		view.PassRate = float64(passed) * 100 / float64(len(view.History))
	}
	return view
}

// Use a new composite-key table, keeping the original table intact for rollback.
func (AstraIQResult) TableName() string { return "model_iq_results" }

// Copy old Astra judgments exactly once; deletion must survive later startups.
func MigrateAstraIQResults(db *gorm.DB) error {
	if err := db.AutoMigrate(&AstraIQResult{}, &AstraIQModelRun{}); err != nil {
		return err
	}
	if !db.Migrator().HasTable("astra_iq_results") {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		marker := Option{Key: "AstraIQModelsMigrated", Value: strconv.FormatInt(common.GetTimestamp(), 10)}
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&marker)
		if claim.Error != nil || claim.RowsAffected == 0 {
			return claim.Error
		}
		var rows []AstraIQResult
		if err := tx.Table("astra_iq_results").Find(&rows).Error; err != nil {
			return err
		}
		for i := range rows {
			rows[i].ModelName = AstraIQModel
			rows[i].ModelKey = AstraIQModelKey(AstraIQModel)
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 100).Error
	})
}

type AstraIQModelRun struct {
	ID         int64  `gorm:"primaryKey" json:"-"`
	ModelKey   string `gorm:"type:varchar(64);index" json:"-"`
	ModelName  string `gorm:"type:varchar(128)" json:"-"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

func GetAstraIQModelRuns(name string) ([]AstraIQRun, error) {
	var rows []AstraIQModelRun
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := DB.WithContext(ctx).Where("model_key = ?", AstraIQModelKey(name)).Order("id DESC").Limit(AstraIQHistorySize).Find(&rows).Error; err != nil {
		return nil, err
	}
	runs := make([]AstraIQRun, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		runs = append(runs, AstraIQRun{StartedAt: rows[i].StartedAt, FinishedAt: rows[i].FinishedAt})
	}
	if name == AstraIQModel {
		legacy, err := GetAstraIQRuns()
		if err != nil {
			return nil, err
		}
		// The one-time migration timestamp separates legacy Astra rounds
		// from later shared tasks, including after per-model run retention.
		var marker Option
		if err := DB.WithContext(ctx).Where(&Option{Key: "AstraIQModelsMigrated"}).Find(&marker).Error; err != nil {
			return nil, err
		}
		cutoff, _ := strconv.ParseInt(marker.Value, 10, 64)
		if cutoff == 0 && len(runs) > 0 {
			cutoff = runs[0].StartedAt
		}

		for i := len(legacy) - 1; i >= 0 && len(runs) < AstraIQHistorySize; i-- {
			if cutoff == 0 || legacy[i].StartedAt < cutoff {
				runs = append([]AstraIQRun{legacy[i]}, runs...)
			}
		}
	}
	return runs, nil
}

func DueAstraIQModels(now, legacyLastRun int64) (map[string]AstraIQSettings, error) {
	config, err := GetAstraIQConfig()
	if err != nil {
		return nil, err
	}
	due := make(map[string]AstraIQSettings)
	for name, settings := range config.Models {
		var run AstraIQModelRun
		if err := DB.Where("model_key = ?", AstraIQModelKey(name)).Order("id DESC").Limit(1).Find(&run).Error; err != nil {
			return nil, err
		}
		last := run.StartedAt
		if last == 0 && name == AstraIQModel {
			last = legacyLastRun
		}
		if settings.Due(now, last) {
			due[name] = settings
		}
	}
	return due, nil
}

func FinishAstraIQModelRun(run AstraIQModelRun) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&run).Update("finished_at", common.GetTimestamp()).Error; err != nil {
			return err
		}
		var kept []int64
		if err := tx.Model(&AstraIQModelRun{}).Where("model_key = ?", AstraIQModelKey(run.ModelName)).Order("id DESC").Limit(AstraIQHistorySize).Pluck("id", &kept).Error; err != nil {
			return err
		}
		return tx.Where("model_key = ? AND id NOT IN ?", AstraIQModelKey(run.ModelName), kept).Delete(&AstraIQModelRun{}).Error
	})
}

// A stable ASCII key keeps model identity case-sensitive even on MySQL's
// default case-insensitive collation, without dialect-specific column types.
func AstraIQModelKey(name string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(name))) }

func (r *AstraIQModelRun) BeforeSave(_ *gorm.DB) error {
	r.ModelKey = AstraIQModelKey(r.ModelName)
	return nil
}
