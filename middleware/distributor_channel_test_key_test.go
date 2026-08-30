package middleware

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupContextForAutoDisabledChannelTestKeyUsesExactCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(nil)
	channel := &model.Channel{
		Id:   7,
		Name: "recovery-test",
		Key:  "disabled-key\nenabled-key",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusAutoDisabled,
			},
		},
	}

	apiErr := SetupContextForAutoDisabledChannelTestKey(context, channel, "gpt-test", 0)

	require.Nil(t, apiErr)
	assert.Equal(t, "disabled-key", common.GetContextKeyString(context, constant.ContextKeyChannelKey))
	assert.Equal(t, 0, common.GetContextKeyInt(context, constant.ContextKeyChannelMultiKeyIndex))
	assert.True(t, common.GetContextKeyBool(context, constant.ContextKeyChannelIsMultiKey))
}

func TestSetupContextForAutoDisabledChannelTestKeyRejectsInvalidIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(nil)
	channel := &model.Channel{
		Id:  7,
		Key: "only-key",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
		},
	}

	apiErr := SetupContextForAutoDisabledChannelTestKey(context, channel, "gpt-test", 1)

	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Error(), "channel key index 1 is unavailable")
}

func TestSetupContextForAutoDisabledChannelTestKeyRejectsManualDisable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(nil)
	channel := &model.Channel{
		Id:  7,
		Key: "manual-key",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey: true,
			MultiKeyStatusList: map[int]int{
				0: common.ChannelStatusManuallyDisabled,
			},
		},
	}

	apiErr := SetupContextForAutoDisabledChannelTestKey(context, channel, "gpt-test", 0)

	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Error(), "channel key index 0 is not auto-disabled")
}
