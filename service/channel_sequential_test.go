package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/require"
)

func TestSequentialKeyAutoSkip(t *testing.T) {
	sequential := &model.Channel{ChannelInfo: model.ChannelInfo{
		IsMultiKey:   true,
		MultiKeyMode: constant.MultiKeyModeSequential,
	}}
	nonSequential := &model.Channel{ChannelInfo: model.ChannelInfo{
		IsMultiKey:   true,
		MultiKeyMode: constant.MultiKeyModePolling,
	}}

	require.True(t, SequentialKeyAutoSkip(sequential))
	require.False(t, SequentialKeyAutoSkip(nonSequential))
	require.False(t, SequentialKeyAutoSkip(nil))
}

func TestShouldDisableChannelForChannelSequentialIgnoresGlobalSwitch(t *testing.T) {
	previous := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = false
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = previous
	})

	sequential := &model.Channel{ChannelInfo: model.ChannelInfo{
		IsMultiKey:   true,
		MultiKeyMode: constant.MultiKeyModeSequential,
	}}
	normal := &model.Channel{ChannelInfo: model.ChannelInfo{
		IsMultiKey:   true,
		MultiKeyMode: constant.MultiKeyModeRandom,
	}}
	unauthorized := types.NewOpenAIError(errors.New("invalid token"), types.ErrorCodeBadResponseStatusCode, http.StatusUnauthorized)
	notDisableStatus := types.NewOpenAIError(errors.New("not found"), types.ErrorCodeBadResponseStatusCode, http.StatusNotFound)

	require.True(t, ShouldDisableChannelForChannel(sequential, unauthorized))
	require.False(t, ShouldDisableChannelForChannel(normal, unauthorized))
	require.False(t, ShouldDisableChannelForChannel(sequential, notDisableStatus))
}

func TestRetryParamSequentialChannelPinDoesNotConsumeChannelRetry(t *testing.T) {
	retry := 2
	param := &RetryParam{Retry: &retry}
	require.False(t, param.HasSequentialChannel())

	for i := 0; i < 3; i++ {
		param.SetSequentialChannel(17)
		param.IncreaseRetry()
		require.Equal(t, 2, param.GetRetry())
		require.True(t, param.HasSequentialChannel())
		require.Equal(t, 17, param.TakeSequentialChannel())
		require.False(t, param.HasSequentialChannel())
	}

	param.IncreaseRetry()
	require.Equal(t, 3, param.GetRetry())
	require.Equal(t, 0, param.TakeSequentialChannel())
}
