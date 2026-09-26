package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	AstraIQModel       = "gpt-6-astra"
	AstraIQClient      = "Codex CLI 0.157.1"
	AstraIQVersion     = "candy-21-medium-v1"
	AstraIQTaskType    = "astra_iq_test"
	AstraIQInterval    = 5 * time.Minute
	AstraIQHistorySize = 24
)

// Separate storage prevents channel edits/imports from overwriting approval.
type AstraIQResult struct {
	ChannelID   int           `json:"channel_id" gorm:"primaryKey;autoIncrement:false"`
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

func AstraIQEnabled() bool {
	return common.GetEnvOrDefaultBool("ASTRA_IQ_ENABLED", false)
}

func AstraIQRequired(modelName string) bool {
	return AstraIQEnabled() && strings.TrimSpace(modelName) == AstraIQModel
}

func ChannelSupportsAstraIQ(ch *Channel) bool {
	if ch == nil {
		return false
	}
	for name := range strings.SplitSeq(ch.Models, ",") {
		if strings.TrimSpace(name) == AstraIQModel {
			return true
		}
	}
	return false
}

func ChannelEligibleForAstraIQ(ch *Channel) bool {
	return ch != nil && ch.Status == common.ChannelStatusEnabled && ChannelSupportsAstraIQ(ch)
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
	return !settings.Enabled || !settings.StopOnFailure || !r.applicable(ch) || (r.Status != "fail" && r.Status != "error")
}

// Read shared storage at selection time so a failed probe takes effect on every
// instance immediately, independently of the ordinary channel cache refresh.
func GetAstraIQResults(ids []int) (map[int]AstraIQResult, error) {
	results := make(map[int]AstraIQResult)
	if len(ids) == 0 {
		return results, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var rows []AstraIQResult
	err := DB.WithContext(ctx).Where("channel_id IN ?", ids).Find(&rows).Error
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
	settings, err := GetAstraIQSettings()
	if err != nil {
		return false
	}
	if !settings.Enabled || !settings.StopOnFailure {
		return true
	}
	results, err := GetAstraIQResults([]int{ch.Id})
	return err == nil && results[ch.Id].Allows(ch, settings)
}

func filterAstraIQCandidateIDs(ids []int, modelName string) []int {
	if !AstraIQRequired(modelName) {
		return ids
	}
	settings, err := GetAstraIQSettings()
	if err != nil {
		return nil
	}
	if !settings.Enabled || !settings.StopOnFailure {
		return ids
	}
	results, err := GetAstraIQResults(ids)
	if err != nil {
		return nil
	}
	kept := make([]int, 0, len(ids))
	for _, id := range ids {
		if results[id].Allows(channelsIDM[id], settings) {
			kept = append(kept, id)
		}
	}
	return kept
}

func SaveAstraIQResult(ch *Channel, result AstraIQResult) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		// Serialize history changes with deletion so a finishing probe cannot
		// write back a copy of history that an administrator just removed.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&AstraIQResult{ChannelID: ch.Id}).Error; err != nil {
			return err
		}
		var previous AstraIQResult
		if err := lockForUpdate(tx).First(&previous, "channel_id = ?", ch.Id).Error; err != nil {
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
func DeleteAstraIQResults(channelID int, checkedAt int64) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var result AstraIQResult
		if err := lockForUpdate(tx).First(&result, "channel_id = ?", channelID).Error; err != nil {
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
