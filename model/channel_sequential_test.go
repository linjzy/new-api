package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
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
