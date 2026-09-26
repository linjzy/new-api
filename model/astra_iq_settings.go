package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm/clause"
)

const astraIQSettingsKey = "AstraIQSettings"

var astraIQTimezone = time.FixedZone("Asia/Shanghai", 8*60*60)

type AstraIQSettings struct {
	Enabled         bool   `json:"enabled"`
	StartTime       string `json:"start_time"`
	EndTime         string `json:"end_time"`
	IntervalMinutes int    `json:"interval_minutes"`
	StopOnFailure   bool   `json:"stop_on_failure"`
}

func DefaultAstraIQSettings() AstraIQSettings {
	return AstraIQSettings{Enabled: true, StartTime: "00:00", EndTime: "00:00", IntervalMinutes: 5, StopOnFailure: true}
}

func (s AstraIQSettings) Validate() error {
	for _, value := range []string{s.StartTime, s.EndTime} {
		parsed, err := time.Parse("15:04", value)
		if err != nil || parsed.Format("15:04") != value {
			return errors.New("Use HH:mm for check times")
		}
	}
	if s.IntervalMinutes < 1 || s.IntervalMinutes > 1440 {
		return errors.New("Check interval must be an integer from 1 to 1440")
	}
	return nil
}

// All nodes use Beijing time. Equal endpoints mean all day; overnight windows
// belong to the day on which they start. The end boundary is exclusive.
func (s AstraIQSettings) window(now time.Time) (time.Time, bool) {
	now = now.In(astraIQTimezone)
	startClock, _ := time.Parse("15:04", s.StartTime)
	endClock, _ := time.Parse("15:04", s.EndTime)
	start := time.Date(now.Year(), now.Month(), now.Day(), startClock.Hour(), startClock.Minute(), 0, 0, astraIQTimezone)
	end := time.Date(now.Year(), now.Month(), now.Day(), endClock.Hour(), endClock.Minute(), 0, 0, astraIQTimezone)
	if !end.After(start) {
		if now.Before(end) {
			start = start.AddDate(0, 0, -1)
		} else {
			end = end.AddDate(0, 0, 1)
		}
	}
	return start, !now.Before(start) && now.Before(end)
}

func (s AstraIQSettings) Active(now time.Time) bool {
	_, active := s.window(now)
	return s.Enabled && active
}

func (s AstraIQSettings) Due(now, lastRun int64) bool {
	start, active := s.window(time.Unix(now, 0))
	return s.Enabled && active && (lastRun == 0 || (s.StartTime != s.EndTime && lastRun < start.Unix()) || now-lastRun >= int64(s.IntervalMinutes)*60)
}

// A single shared database value makes all four fields atomic and immediately
// visible to every routing/scheduler instance, including after a restart.
func GetAstraIQSettings() (AstraIQSettings, error) {
	settings := DefaultAstraIQSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var option Option
	result := DB.WithContext(ctx).Where(&Option{Key: astraIQSettingsKey}).Find(&option)
	if result.Error != nil || result.RowsAffected == 0 {
		return settings, result.Error
	}
	if err := common.UnmarshalJsonStr(option.Value, &settings); err != nil {
		return settings, err
	}
	return settings, settings.Validate()
}

func SaveAstraIQSettings(settings AstraIQSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	value, err := common.Marshal(settings)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"}),
	}).Create(&Option{Key: astraIQSettingsKey, Value: string(value)}).Error
}
