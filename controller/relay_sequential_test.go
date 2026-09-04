package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLockedTaskRetryChannelRefreshesSequentialKeyState(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
	})

	channel := model.Channel{
		Name:   "locked-sequential-task",
		Key:    "first\nsecond",
		Status: common.ChannelStatusEnabled,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:   true,
			MultiKeySize: 2,
			MultiKeyMode: constant.MultiKeyModeSequential,
		},
	}
	require.NoError(t, db.Create(&channel).Error)

	lockedSnapshot, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.True(t, model.UpdateChannelStatusByKeyIndex(
		channel.Id,
		"first",
		0,
		common.ChannelStatusAutoDisabled,
		"provider rejected key",
	))

	staleKey, staleIndex, staleErr := lockedSnapshot.GetNextEnabledKey()
	require.Nil(t, staleErr)
	assert.Equal(t, "first", staleKey)
	assert.Equal(t, 0, staleIndex)

	refreshedChannel, err := resolveLockedTaskRetryChannel(lockedSnapshot, true)
	require.NoError(t, err)
	key, index, apiErr := refreshedChannel.GetNextEnabledKey()
	require.Nil(t, apiErr)
	assert.Equal(t, "second", key)
	assert.Equal(t, 1, index)
}
