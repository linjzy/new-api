package service

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCustomSequentialRetirementDoesNotWaitForNotification(t *testing.T) {
	oldMemory := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = oldMemory })
	require.NoError(t, model.DB.AutoMigrate(&model.Ability{}))
	root := model.User{Username: "custom-notification", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&root).Error)
	channel := model.Channel{Name: "custom-notify", Key: "bad-key\ngood-key", Status: common.ChannelStatusEnabled, Models: "custom-model", Group: "default",
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeSequential},
	}
	require.NoError(t, model.DB.Create(&channel).Error)
	require.NoError(t, model.DB.Create(&model.Ability{ChannelId: channel.Id, Group: "default", Model: "custom-model", Enabled: true}).Error)
	common.MemoryCacheEnabled = true
	model.InitChannelCache()
	started := make(chan struct{})
	release := make(chan struct{})
	queryDone := make(chan struct{})
	var once sync.Once
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register("custom_notification_block", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			once.Do(func() { close(started) })
			<-release
		}
	}))
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register("custom_notification_done", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			close(queryDone)
		}
	}))
	t.Cleanup(func() {
		close(release)
		select {
		case <-queryDone:
		case <-time.After(5 * time.Second):
			t.Error("notification query did not finish")
		}
		_ = model.DB.Callback().Query().Remove("custom_notification_block")
		_ = model.DB.Callback().Query().Remove("custom_notification_done")
		model.DB.Where("channel_id = ?", channel.Id).Delete(&model.Ability{})
		model.DB.Delete(&channel)
		model.DB.Delete(&root)
	})
	result := make(chan bool, 1)
	go func() {
		result <- DisableSequentialKey(types.ChannelError{ChannelId: channel.Id, ChannelName: channel.Name, UsingKey: "bad-key", IsMultiKey: true, AutoBan: true}, 0, "invalid key")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("notification did not start")
	}
	// Notification is deliberately held at the database boundary. Retirement
	// must already be complete; the timeout only detects a deadlock.
	select {
	case changed := <-result:
		require.True(t, changed)
	case <-time.After(5 * time.Second):
		t.Fatal("retirement waited for notification")
	}
	cached, err := model.CacheGetChannel(channel.Id)
	require.NoError(t, err)
	key, index, apiErr := cached.GetNextEnabledKey()
	require.Nil(t, apiErr)
	assert.Equal(t, "good-key", key)
	assert.Equal(t, 1, index)
}
