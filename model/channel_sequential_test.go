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
