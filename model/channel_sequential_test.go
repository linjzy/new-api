package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetNextEnabledKeySequentialUsesFirstEnabledKey(t *testing.T) {
	channel := &Channel{
		Key: "first\nsecond\nthird",
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeyMode: constant.MultiKeyModeSequential,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				2: common.ChannelStatusAutoDisabled,
			},
		},
	}

	key, index, err := channel.GetNextEnabledKey()
	require.Nil(t, err)
	require.Equal(t, "second", key)
	require.Equal(t, 1, index)

	// Sequential selection is stable until the failed key is explicitly
	// retired; it must not advance a cursor as polling mode does.
	key, index, err = channel.GetNextEnabledKey()
	require.Nil(t, err)
	require.Equal(t, "second", key)
	require.Equal(t, 1, index)
}

func TestAutoDisabledKeyIndexesOnlyReturnsValidAutomaticEntries(t *testing.T) {
	channel := &Channel{
		Key: "first\nsecond\nthird",
		ChannelInfo: ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				-1: common.ChannelStatusAutoDisabled,
				0:  common.ChannelStatusAutoDisabled,
				1:  common.ChannelStatusManuallyDisabled,
				2:  common.ChannelStatusAutoDisabled,
				3:  common.ChannelStatusAutoDisabled,
			},
		},
	}

	assert.Equal(t, []int{0, 2}, channel.AutoDisabledKeyIndexes())
}

func TestUpdateChannelStatusByKeyIndexDistinguishesDuplicateKeys(t *testing.T) {
	setupChannelStatusTest(t)

	channel := Channel{
		Name:   "sequential-duplicate-keys",
		Key:    "same\nsame",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
			MultiKeyMode: constant.MultiKeyModeSequential,
		},
	}
	require.NoError(t, DB.Create(&channel).Error)
	require.False(t, UpdateChannelStatusByKeyIndex(
		channel.Id,
		"same",
		2,
		common.ChannelStatusAutoDisabled,
		"stale key index",
	))

	require.True(t, UpdateChannelStatusByKeyIndex(
		channel.Id,
		"same",
		0,
		common.ChannelStatusAutoDisabled,
		"first credential rejected",
	))
	refreshed, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	key, index, apiErr := refreshed.GetNextEnabledKey()
	require.Nil(t, apiErr)
	assert.Equal(t, "same", key)
	assert.Equal(t, 1, index)

	require.True(t, UpdateChannelStatusByKeyIndex(
		channel.Id,
		"same",
		1,
		common.ChannelStatusAutoDisabled,
		"second credential rejected",
	))
	refreshed, err = GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, refreshed.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, refreshed.ChannelInfo.MultiKeyStatusList[0])
	assert.Equal(t, common.ChannelStatusAutoDisabled, refreshed.ChannelInfo.MultiKeyStatusList[1])
}

func TestEnableAutoDisabledChannelKeyClearsOnlyExactAutomaticEntry(t *testing.T) {
	setupChannelStatusTest(t)

	channel := Channel{
		Name:   "recover-one-key",
		Key:    "first\nsecond\nthird",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 3,
			MultiKeyMode: constant.MultiKeyModeSequential,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusManuallyDisabled,
				2: common.ChannelStatusAutoDisabled,
			},
			MultiKeyDisabledReason: map[int]string{
				0: "first rejected",
				1: "operator disabled",
				2: "third rejected",
			},
			MultiKeyDisabledTime: map[int]int64{
				0: 100,
				1: 200,
				2: 300,
			},
		},
	}
	require.NoError(t, DB.Create(&channel).Error)

	require.False(t, EnableAutoDisabledChannelKey(channel.Id, 0, "first", 99))
	require.False(t, EnableAutoDisabledChannelKey(channel.Id, 1, "second", 200))
	require.True(t, EnableAutoDisabledChannelKey(channel.Id, 0, "first", 100))

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 0)
	assert.NotContains(t, stored.ChannelInfo.MultiKeyDisabledReason, 0)
	assert.NotContains(t, stored.ChannelInfo.MultiKeyDisabledTime, 0)
	assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.ChannelInfo.MultiKeyStatusList[1])
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[2])
	assert.Equal(t, "operator disabled", stored.ChannelInfo.MultiKeyDisabledReason[1])
	assert.Equal(t, int64(300), stored.ChannelInfo.MultiKeyDisabledTime[2])
}

func TestEnableAutoDisabledChannelKeyRestoresFullyDisabledChannel(t *testing.T) {
	setupChannelStatusTest(t)

	channel := Channel{
		Name:      "recover-disabled-channel",
		Key:       "first\nsecond",
		Models:    "gpt-test",
		Group:     "default",
		Status:    common.ChannelStatusAutoDisabled,
		OtherInfo: `{"status_reason":"All keys are disabled","status_time":123,"preserved":"value"}`,
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
			MultiKeyMode: constant.MultiKeyModeSequential,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
				1: common.ChannelStatusAutoDisabled,
			},
			MultiKeyDisabledReason: map[int]string{0: "first rejected", 1: "second rejected"},
			MultiKeyDisabledTime:   map[int]int64{0: 100, 1: 200},
		},
	}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, DB.Create(&Ability{
		Group:     channel.Group,
		Model:     channel.Models,
		ChannelId: channel.Id,
		Enabled:   false,
	}).Error)

	require.True(t, EnableAutoDisabledChannelKey(channel.Id, 1, "second", 200))

	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
	assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
	assert.NotContains(t, stored.ChannelInfo.MultiKeyStatusList, 1)
	otherInfo := stored.GetOtherInfo()
	assert.NotContains(t, otherInfo, "status_reason")
	assert.NotContains(t, otherInfo, "status_time")
	assert.Equal(t, "value", otherInfo["preserved"])

	var ability Ability
	require.NoError(t, DB.Where("channel_id = ?", channel.Id).First(&ability).Error)
	assert.True(t, ability.Enabled)
}
