package service

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	disableChannel(channelError, reason)
}

func disableChannel(channelError types.ChannelError, reason string) bool {
	common.SysLog(fmt.Sprintf("通道「%s」（#%d）发生错误，准备禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(fmt.Sprintf("通道「%s」（#%d）未启用自动禁用功能，跳过禁用操作", channelError.ChannelName, channelError.ChannelId))
		return false
	}

	success := model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason)
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelError.ChannelName, channelError.ChannelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, reason)
		NotifyRootUser(formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled), subject, content)
	}
	return success
}

func EnableChannel(channelId int, usingKey string, channelName string) {
	success := model.UpdateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "")
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		NotifyRootUser(formatNotifyType(channelId, common.ChannelStatusEnabled), subject, content)
	}
}

func shouldDisableChannelCore(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	if operation_setting.ShouldDisableByStatusCode(err.StatusCode) {
		return true
	}

	lowerMessage := strings.ToLower(err.Error())
	search, _ := AcSearch(lowerMessage, operation_setting.AutomaticDisableKeywords, true)
	return search
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	return shouldDisableChannelCore(err)
}

// SequentialKeyAutoSkip reports whether a channel uses sequential multi-key
// fallback. The nil check matters on the first relay attempt, where the
// distributor has already selected a channel but relay metadata is not built
// yet and the retry loop may only have a lightweight channel placeholder.
func SequentialKeyAutoSkip(channel *model.Channel) bool {
	return channel != nil && channel.ChannelInfo.IsMultiKey &&
		channel.ChannelInfo.MultiKeyMode == constant.MultiKeyModeSequential
}

// ShouldDisableChannelForChannel decides whether a failed request should
// disable the channel (or, in sequential multi-key mode, the used key).
// Sequential mode deliberately evaluates the status/keyword rules directly:
// retiring a bad key is the mode's contract and must not depend on the global
// channel auto-disable switch.
func ShouldDisableChannelForChannel(channel *model.Channel, err *types.NewAPIError) bool {
	if SequentialKeyAutoSkip(channel) {
		return shouldDisableChannelCore(err)
	}
	return ShouldDisableChannel(err)
}

// DisableSequentialKey retires a failing key synchronously. The next retry is
// selected immediately after this call, so an asynchronous update could pick
// the same invalid key again.
func DisableSequentialKey(channelError types.ChannelError, reason string) bool {
	if !channelError.IsMultiKey {
		return false
	}
	return disableChannel(channelError, reason)
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if newAPIError != nil {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	return true
}
