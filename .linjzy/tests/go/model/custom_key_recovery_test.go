package model

import (
	"os"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func customRecoveryDatabase(t *testing.T) {
	t.Helper()
	oldDB, oldLog := DB, LOG_DB
	oldMemory := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	if dsn := os.Getenv("CUSTOM_TEST_SQL_DSN"); dsn != "" {
		var dialector gorm.Dialector
		switch os.Getenv("CUSTOM_TEST_SQL_DRIVER") {
		case "postgres":
			dialector = postgres.Open(dsn)
			common.SetDatabaseTypes(common.DatabaseTypePostgreSQL, common.DatabaseTypePostgreSQL)
		case "mysql":
			dialector = mysql.Open(dsn)
			common.SetDatabaseTypes(common.DatabaseTypeMySQL, common.DatabaseTypeMySQL)
		default:
			t.Fatal("CUSTOM_TEST_SQL_DRIVER must be postgres or mysql")
		}
		var err error
		DB, err = gorm.Open(dialector, &gorm.Config{})
		require.NoError(t, err)
		LOG_DB = DB
		initCol()
		require.NoError(t, DB.AutoMigrate(&Channel{}, &Ability{}))
		sqlDB, err := DB.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	require.NoError(t, DB.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&Ability{}).Error)
	require.NoError(t, DB.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&Channel{}).Error)
	t.Cleanup(func() {
		DB, LOG_DB = oldDB, oldLog
		common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
		initCol()
		common.MemoryCacheEnabled = oldMemory
		channelSyncLock.Lock()
		channelsIDM = nil
		group2model2channels = nil
		channelSyncLock.Unlock()
	})
}

func TestCustomKeyRecoveryRestoresRoutingAndPreservesCursor(t *testing.T) {
	customRecoveryDatabase(t)
	for _, mode := range []constant.MultiKeyMode{constant.MultiKeyModeSequential, constant.MultiKeyModePolling} {
		t.Run(string(mode), func(t *testing.T) {
			channel := Channel{Name: "recover-" + string(mode), Key: "key-a\nkey-b", Group: "default", Models: "custom-model", Status: common.ChannelStatusAutoDisabled,
				ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: mode, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled, 1: common.ChannelStatusManuallyDisabled}, MultiKeyDisabledTime: map[int]int64{0: 123}, MultiKeyDisabledReason: map[int]string{0: "capacity"}},
			}
			require.NoError(t, DB.Create(&channel).Error)
			require.NoError(t, DB.Create(&Ability{ChannelId: channel.Id, Group: "default", Model: "custom-model", Enabled: false}).Error)
			common.MemoryCacheEnabled = true
			InitChannelCache()
			cached, err := CacheGetChannel(channel.Id)
			require.NoError(t, err)
			cached.ChannelInfo.MultiKeyPollingIndex = 1
			require.True(t, EnableAutoDisabledChannelKey(channel.Id, 0, "key-a", 123))
			cached, err = CacheGetChannel(channel.Id)
			require.NoError(t, err)
			assert.Equal(t, common.ChannelStatusEnabled, cached.Status)
			if mode == constant.MultiKeyModePolling {
				assert.Equal(t, 1, cached.ChannelInfo.MultiKeyPollingIndex)
			}
			key, index, apiErr := cached.GetNextEnabledKey()
			require.Nil(t, apiErr)
			assert.Equal(t, "key-a", key)
			assert.Equal(t, 0, index)
			candidates, err := GetRandomSatisfiedChannel("default", "custom-model", 0, nil)
			require.NoError(t, err)
			require.NotNil(t, candidates)
			var ability Ability
			require.NoError(t, DB.Where("channel_id = ?", channel.Id).First(&ability).Error)
			assert.True(t, ability.Enabled)
			var stored Channel
			require.NoError(t, DB.First(&stored, channel.Id).Error)
			assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.ChannelInfo.MultiKeyStatusList[1])
			assert.NotContains(t, stored.ChannelInfo.MultiKeyDisabledReason, 0)
			assert.False(t, EnableAutoDisabledChannelKey(channel.Id, 0, "key-a", 123))
			common.MemoryCacheEnabled = false
		})
	}
}

func TestCustomKeyRecoveryRejectsStaleResultAndSerializesSameKey(t *testing.T) {
	customRecoveryDatabase(t)
	channel := Channel{Name: "recover-race", Key: "same\nsame", Status: common.ChannelStatusEnabled,
		ChannelInfo: ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModeSequential, MultiKeyStatusList: map[int]int{1: common.ChannelStatusAutoDisabled}, MultiKeyDisabledTime: map[int]int64{1: 234}},
	}
	require.NoError(t, DB.Create(&channel).Error)
	assert.False(t, EnableAutoDisabledChannelKey(channel.Id, 1, "replaced", 234))
	assert.False(t, EnableAutoDisabledChannelKey(channel.Id, 1, "same", 123))
	start := make(chan struct{})
	results := make(chan bool, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- EnableAutoDisabledChannelKey(channel.Id, 1, "same", 234) }()
	}
	close(start)
	wg.Wait()
	close(results)
	count := 0
	for changed := range results {
		if changed {
			count++
		}
	}
	assert.Equal(t, 1, count)
	stored, err := GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Empty(t, stored.ChannelInfo.MultiKeyStatusList)
}
