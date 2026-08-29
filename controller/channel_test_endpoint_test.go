package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/stretchr/testify/require"
)

func TestResolveChannelTestRequestPathUsesResponsesCompatibilityPolicyForChat(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	previous := settings.ChatCompletionsToResponsesPolicy
	previousPassThrough := settings.PassThroughRequestEnabled
	settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{
		Enabled:       true,
		ChannelIDs:    []int{4},
		ModelPatterns: []string{".*"},
	}
	settings.PassThroughRequestEnabled = false
	t.Cleanup(func() {
		settings.ChatCompletionsToResponsesPolicy = previous
		settings.PassThroughRequestEnabled = previousPassThrough
	})

	channel := &model.Channel{Id: 4, Type: constant.ChannelTypeOpenAI}
	require.Equal(t, "/v1/responses", resolveChannelTestRequestPath(channel, "gpt-5.6-sol", ""))
	require.Equal(t, "/v1/embeddings", resolveChannelTestRequestPath(channel, "text-embedding-3-small", ""))
	require.Equal(t, "/v1/rerank", resolveChannelTestRequestPath(channel, "cohere-rerank-v3", ""))
}

func TestResolveChannelTestRequestPathHonorsPassThroughAndExplicitEndpoint(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	previous := settings.ChatCompletionsToResponsesPolicy
	previousPassThrough := settings.PassThroughRequestEnabled
	settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{
		Enabled:       true,
		ChannelIDs:    []int{4},
		ModelPatterns: []string{".*"},
	}
	t.Cleanup(func() {
		settings.ChatCompletionsToResponsesPolicy = previous
		settings.PassThroughRequestEnabled = previousPassThrough
	})

	channel := &model.Channel{Id: 4, Type: constant.ChannelTypeOpenAI}
	settings.PassThroughRequestEnabled = true
	require.Equal(t, "/v1/chat/completions", resolveChannelTestRequestPath(channel, "gpt-5.6-sol", ""))

	settings.PassThroughRequestEnabled = false
	channelSetting := dto.ChannelSettings{PassThroughBodyEnabled: true}
	channel.SetSetting(channelSetting)
	require.Equal(t, "/v1/chat/completions", resolveChannelTestRequestPath(channel, "gpt-5.6-sol", ""))

	require.Equal(t, "/v1/chat/completions", resolveChannelTestRequestPath(
		channel,
		"gpt-5.6-sol",
		string(constant.EndpointTypeOpenAI),
	))
	require.Equal(t, string(constant.EndpointTypeOpenAI), normalizeChannelTestEndpoint(channel, string(constant.EndpointTypeOpenAI)))
	require.Equal(t, string(constant.EndpointTypeOpenAIResponse), normalizeChannelTestEndpoint(
		&model.Channel{Type: constant.ChannelTypeCodex},
		"",
	))
}
