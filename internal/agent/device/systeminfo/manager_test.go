package systeminfo

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/flightctl/flightctl/api/core/v1beta1"
	"github.com/flightctl/flightctl/internal/agent/config"
	"github.com/flightctl/flightctl/internal/agent/device/fileio"
	"github.com/flightctl/flightctl/internal/agent/device/status"
	"github.com/flightctl/flightctl/internal/agent/device/systeminfo/common"
	"github.com/flightctl/flightctl/internal/util"
	"github.com/flightctl/flightctl/pkg/executer"
	"github.com/flightctl/flightctl/pkg/log"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
)

func TestManager(t *testing.T) {
	require := require.New(t)

	// setup
	tmpDir := t.TempDir()
	dataDir := filepath.Join("etc", "flightctl")
	readWriter := fileio.NewReadWriter(
		fileio.NewReader(fileio.WithReaderRootDir(tmpDir)),
		fileio.NewWriter(fileio.WithWriterRootDir(tmpDir)),
	)
	err := readWriter.MkdirAll(dataDir, 0755)
	require.NoError(err)
	err = readWriter.MkdirAll("/proc/sys/kernel/random", 0755)
	require.NoError(err)
	log := log.NewPrefixLogger("test")

	// set mock boot_id
	mockBootID := "c4070599-f0f0-472d-8084-09b7274ebf18"
	err = readWriter.WriteFile(bootIDPath, []byte(mockBootID), 0644)
	require.NoError(err)

	ctrl := gomock.NewController(t)
	mockExecuter := executer.NewMockExecuter(ctrl)
	bootTime := "2024-12-13 11:01:08"
	collectTimeout := util.Duration(5 * time.Second)
	mockExecuter.EXPECT().ExecuteWithContext(gomock.Any(), "uptime", "-s").Return(bootTime, "", 0).MinTimes(2)

	// initialize client new device
	manager := NewManager(log, mockExecuter, readWriter, dataDir, nil, nil, collectTimeout, 0)
	err = manager.Initialize(context.Background())
	require.NoError(err)
	require.NotNil(manager)
	require.NotEmpty(manager.BootTime())
	require.False(manager.IsRebooted())
	require.Equal(mockBootID, manager.BootID())

	// test rebooted
	// change bootID stored in system.json on disk
	mockBootID2 := "c4070599-f0f0-472d-8084-09b7274ebf19"
	mockStatus := &Boot{
		Time: bootTime,
		ID:   mockBootID2,
	}
	mockStatusBytes, err := json.Marshal(mockStatus)
	require.NoError(err)
	err = readWriter.WriteFile(filepath.Join(dataDir, SystemFileName), mockStatusBytes, 0644)
	require.NoError(err)

	// reinitialize client
	manager = NewManager(log, mockExecuter, readWriter, dataDir, nil, nil, collectTimeout, 0)
	err = manager.Initialize(context.Background())
	require.NoError(err)
	require.NotEmpty(manager.BootTime())
	require.Equal(mockBootID, manager.BootID())
	require.True(manager.IsRebooted())
}

func TestRun(t *testing.T) {
	t.Run("When Run is called it should collect immediately and cache results", func(t *testing.T) {
		require := require.New(t)

		tmpDir := t.TempDir()
		dataDir := filepath.Join("etc", "flightctl")
		readWriter := fileio.NewReadWriter(
			fileio.NewReader(fileio.WithReaderRootDir(tmpDir)),
			fileio.NewWriter(fileio.WithWriterRootDir(tmpDir)),
		)
		err := readWriter.MkdirAll(dataDir, 0755)
		require.NoError(err)
		err = readWriter.MkdirAll("/proc/sys/kernel/random", 0755)
		require.NoError(err)

		mockBootID := "c4070599-f0f0-472d-8084-09b7274ebf18"
		err = readWriter.WriteFile(bootIDPath, []byte(mockBootID), 0644)
		require.NoError(err)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockExecuter := executer.NewMockExecuter(ctrl)
		bootTime := "2024-12-13 11:01:08"
		collectTimeout := util.Duration(5 * time.Second)
		// Once for Initialize boot time, once for Run's initial collect
		mockExecuter.EXPECT().ExecuteWithContext(gomock.Any(), "uptime", "-s").Return(bootTime, "", 0).MinTimes(2)

		log := log.NewPrefixLogger("test")

		manager := NewManager(log, mockExecuter, readWriter, dataDir, nil, nil, collectTimeout, 0)
		err = manager.Initialize(context.Background())
		require.NoError(err)

		// After Initialize but before Run, no cached info yet
		require.Nil(manager.cachedSystemInfo)

		// With interval=0, Run collects once and returns
		manager.Run(context.Background())

		// After Run, cached info should be populated
		require.NotNil(manager.cachedSystemInfo)
		require.Equal(mockBootID, manager.cachedSystemInfo.BootID)
	})

	t.Run("When Run is called with interval it should collect periodically", func(t *testing.T) {
		require := require.New(t)

		tmpDir := t.TempDir()
		dataDir := filepath.Join("etc", "flightctl")
		readWriter := fileio.NewReadWriter(
			fileio.NewReader(fileio.WithReaderRootDir(tmpDir)),
			fileio.NewWriter(fileio.WithWriterRootDir(tmpDir)),
		)
		err := readWriter.MkdirAll(dataDir, 0755)
		require.NoError(err)
		err = readWriter.MkdirAll("/proc/sys/kernel/random", 0755)
		require.NoError(err)

		mockBootID := "c4070599-f0f0-472d-8084-09b7274ebf18"
		err = readWriter.WriteFile(bootIDPath, []byte(mockBootID), 0644)
		require.NoError(err)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockExecuter := executer.NewMockExecuter(ctrl)
		bootTime := "2024-12-13 11:01:08"
		collectTimeout := util.Duration(5 * time.Second)
		// Initial + periodic collections
		mockExecuter.EXPECT().ExecuteWithContext(gomock.Any(), "uptime", "-s").Return(bootTime, "", 0).MinTimes(3)

		log := log.NewPrefixLogger("test")

		// Short interval for test
		collectionInterval := util.Duration(50 * time.Millisecond)
		manager := NewManager(log, mockExecuter, readWriter, dataDir, nil, nil, collectTimeout, collectionInterval)
		err = manager.Initialize(context.Background())
		require.NoError(err)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			manager.Run(ctx)
			close(done)
		}()

		// Wait for at least 2 collection cycles
		time.Sleep(150 * time.Millisecond)
		cancel()
		<-done

		require.NotNil(manager.cachedSystemInfo)
	})

	t.Run("When context is cancelled it should stop Run", func(t *testing.T) {
		require := require.New(t)

		tmpDir := t.TempDir()
		dataDir := filepath.Join("etc", "flightctl")
		readWriter := fileio.NewReadWriter(
			fileio.NewReader(fileio.WithReaderRootDir(tmpDir)),
			fileio.NewWriter(fileio.WithWriterRootDir(tmpDir)),
		)
		err := readWriter.MkdirAll(dataDir, 0755)
		require.NoError(err)
		err = readWriter.MkdirAll("/proc/sys/kernel/random", 0755)
		require.NoError(err)

		mockBootID := "c4070599-f0f0-472d-8084-09b7274ebf18"
		err = readWriter.WriteFile(bootIDPath, []byte(mockBootID), 0644)
		require.NoError(err)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockExecuter := executer.NewMockExecuter(ctrl)
		bootTime := "2024-12-13 11:01:08"
		collectTimeout := util.Duration(5 * time.Second)
		mockExecuter.EXPECT().ExecuteWithContext(gomock.Any(), "uptime", "-s").Return(bootTime, "", 0).MinTimes(1)

		log := log.NewPrefixLogger("test")

		collectionInterval := util.Duration(1 * time.Second)
		manager := NewManager(log, mockExecuter, readWriter, dataDir, nil, nil, collectTimeout, collectionInterval)
		err = manager.Initialize(context.Background())
		require.NoError(err)

		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			manager.Run(ctx)
			close(done)
		}()

		// Cancel quickly — Run should exit promptly
		time.Sleep(10 * time.Millisecond)
		cancel()

		select {
		case <-done:
			// Run exited as expected
		case <-time.After(2 * time.Second):
			require.Fail("Run did not stop after context cancellation")
		}
	})
}

func TestStatusReturnsCachedResults(t *testing.T) {
	t.Run("When Status is called before Run it should lazy-collect and return populated info", func(t *testing.T) {
		require := require.New(t)

		tmpDir := t.TempDir()
		dataDir := filepath.Join("etc", "flightctl")
		readWriter := fileio.NewReadWriter(
			fileio.NewReader(fileio.WithReaderRootDir(tmpDir)),
			fileio.NewWriter(fileio.WithWriterRootDir(tmpDir)),
		)
		err := readWriter.MkdirAll(dataDir, 0755)
		require.NoError(err)
		err = readWriter.MkdirAll("/proc/sys/kernel/random", 0755)
		require.NoError(err)

		mockBootID := "c4070599-f0f0-472d-8084-09b7274ebf18"
		err = readWriter.WriteFile(bootIDPath, []byte(mockBootID), 0644)
		require.NoError(err)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockExecuter := executer.NewMockExecuter(ctrl)
		bootTime := "2024-12-13 11:01:08"
		collectTimeout := util.Duration(5 * time.Second)
		// Once for Initialize boot time, once for lazy collect in Status
		mockExecuter.EXPECT().ExecuteWithContext(gomock.Any(), "uptime", "-s").Return(bootTime, "", 0).MinTimes(2)

		log := log.NewPrefixLogger("test")

		manager := NewManager(log, mockExecuter, readWriter, dataDir, nil, nil, collectTimeout, 0)
		err = manager.Initialize(context.Background())
		require.NoError(err)

		// Cache is nil before Run — Status should lazy-collect
		require.Nil(manager.cachedSystemInfo)

		deviceStatus := &v1beta1.DeviceStatus{}
		err = manager.Status(context.Background(), deviceStatus)
		require.NoError(err)
		require.Equal(mockBootID, deviceStatus.SystemInfo.BootID)
		require.NotEmpty(deviceStatus.SystemInfo.AgentVersion)

		// After Status, cache should now be populated
		require.NotNil(manager.cachedSystemInfo)
	})

	t.Run("When Status is called after Run it should return cached results", func(t *testing.T) {
		require := require.New(t)

		tmpDir := t.TempDir()
		dataDir := filepath.Join("etc", "flightctl")
		readWriter := fileio.NewReadWriter(
			fileio.NewReader(fileio.WithReaderRootDir(tmpDir)),
			fileio.NewWriter(fileio.WithWriterRootDir(tmpDir)),
		)
		err := readWriter.MkdirAll(dataDir, 0755)
		require.NoError(err)
		err = readWriter.MkdirAll("/proc/sys/kernel/random", 0755)
		require.NoError(err)

		mockBootID := "c4070599-f0f0-472d-8084-09b7274ebf18"
		err = readWriter.WriteFile(bootIDPath, []byte(mockBootID), 0644)
		require.NoError(err)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockExecuter := executer.NewMockExecuter(ctrl)
		bootTime := "2024-12-13 11:01:08"
		collectTimeout := util.Duration(5 * time.Second)
		// Once for Initialize boot time, once for Run's collect
		mockExecuter.EXPECT().ExecuteWithContext(gomock.Any(), "uptime", "-s").Return(bootTime, "", 0).MinTimes(2)

		log := log.NewPrefixLogger("test")

		manager := NewManager(log, mockExecuter, readWriter, dataDir, nil, nil, collectTimeout, 0)
		err = manager.Initialize(context.Background())
		require.NoError(err)

		// Run collects (with interval=0, collects once and returns)
		manager.Run(context.Background())

		// Status should return cached results without re-collecting
		deviceStatus := &v1beta1.DeviceStatus{}
		err = manager.Status(context.Background(), deviceStatus)
		require.NoError(err)
		require.Equal(mockBootID, deviceStatus.SystemInfo.BootID)
		require.NotEmpty(deviceStatus.SystemInfo.AgentVersion)
	})

	t.Run("When Status is called with WithForceCollect it should re-collect even when cache is populated", func(t *testing.T) {
		require := require.New(t)

		tmpDir := t.TempDir()
		dataDir := filepath.Join("etc", "flightctl")
		readWriter := fileio.NewReadWriter(
			fileio.NewReader(fileio.WithReaderRootDir(tmpDir)),
			fileio.NewWriter(fileio.WithWriterRootDir(tmpDir)),
		)
		err := readWriter.MkdirAll(dataDir, 0755)
		require.NoError(err)
		err = readWriter.MkdirAll("/proc/sys/kernel/random", 0755)
		require.NoError(err)

		mockBootID := "c4070599-f0f0-472d-8084-09b7274ebf18"
		err = readWriter.WriteFile(bootIDPath, []byte(mockBootID), 0644)
		require.NoError(err)

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockExecuter := executer.NewMockExecuter(ctrl)
		bootTime := "2024-12-13 11:01:08"
		collectTimeout := util.Duration(5 * time.Second)
		// Once for Initialize, once for Run's collect, once for Force re-collect
		mockExecuter.EXPECT().ExecuteWithContext(gomock.Any(), "uptime", "-s").Return(bootTime, "", 0).MinTimes(3)

		log := log.NewPrefixLogger("test")

		manager := NewManager(log, mockExecuter, readWriter, dataDir, nil, nil, collectTimeout, 0)
		err = manager.Initialize(context.Background())
		require.NoError(err)

		// Run collects once (interval=0), populating the cache
		manager.Run(context.Background())
		require.NotNil(manager.cachedSystemInfo)
		firstCached := manager.cachedSystemInfo

		// Normal Status should return cached results (no re-collection)
		deviceStatus := &v1beta1.DeviceStatus{}
		err = manager.Status(context.Background(), deviceStatus)
		require.NoError(err)
		require.Equal(firstCached, manager.cachedSystemInfo)

		// Status with WithForceCollect should trigger a fresh collection
		deviceStatus2 := &v1beta1.DeviceStatus{}
		err = manager.Status(context.Background(), deviceStatus2, status.WithForceCollect())
		require.NoError(err)
		require.NotNil(manager.cachedSystemInfo)
		// The cache pointer should have been replaced by a new collection
		require.NotSame(firstCached, manager.cachedSystemInfo)
		require.Equal(mockBootID, deviceStatus2.SystemInfo.BootID)
		require.NotEmpty(deviceStatus2.SystemInfo.AgentVersion)
	})
}

func TestReloadConfig(t *testing.T) {
	tests := []struct {
		name        string
		initialKeys []string
		newKeys     []string
		expected    bool
	}{
		{
			name:        "no change in info keys",
			initialKeys: []string{common.NetInterfaceDefaultKey, common.CPUCoresKey},
			newKeys:     []string{common.NetInterfaceDefaultKey, common.CPUCoresKey},
			expected:    true,
		},
		{
			name:        "change within same collector type - network",
			initialKeys: []string{common.NetInterfaceDefaultKey},
			newKeys:     []string{common.NetMACDefaultKey},
			expected:    true,
		},
		{
			name:        "change within same collector type - CPU",
			initialKeys: []string{common.CPUCoresKey},
			newKeys:     []string{common.CPUModelKey},
			expected:    true,
		},
		{
			name:        "change to different collector type",
			initialKeys: []string{common.CPUCoresKey},
			newKeys:     []string{common.MemoryTotalKbKey},
			expected:    false,
		},
		{
			name:        "add key requiring same collector",
			initialKeys: []string{common.NetInterfaceDefaultKey},
			newKeys:     []string{common.NetInterfaceDefaultKey, common.NetMACDefaultKey},
			expected:    true,
		},
		{
			name:        "add key requiring different collector",
			initialKeys: []string{common.CPUCoresKey},
			newKeys:     []string{common.CPUCoresKey, common.GPUKey},
			expected:    false,
		},
		{
			name:        "remove key but keep collector active",
			initialKeys: []string{common.NetInterfaceDefaultKey, common.NetMACDefaultKey},
			newKeys:     []string{common.NetInterfaceDefaultKey},
			expected:    true,
		},
		{
			name:        "remove key that disables collector",
			initialKeys: []string{common.CPUCoresKey, common.MemoryTotalKbKey},
			newKeys:     []string{common.CPUCoresKey},
			expected:    true,
		},
		{
			name:        "empty to non-empty",
			initialKeys: []string{},
			newKeys:     []string{common.CPUCoresKey},
			expected:    false,
		},
		{
			name:        "non-empty to empty",
			initialKeys: []string{common.CPUCoresKey},
			newKeys:     []string{},
			expected:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)

			tmpDir := t.TempDir()
			dataDir := filepath.Join("etc", "flightctl")
			readWriter := fileio.NewReadWriter(
				fileio.NewReader(fileio.WithReaderRootDir(tmpDir)),
				fileio.NewWriter(fileio.WithWriterRootDir(tmpDir)),
			)
			err := readWriter.MkdirAll(dataDir, 0755)
			require.NoError(err)

			ctrl := gomock.NewController(t)
			mockExecuter := executer.NewMockExecuter(ctrl)
			log := log.NewPrefixLogger("test")
			collectTimeout := util.Duration(5 * time.Second)

			manager := NewManager(log, mockExecuter, readWriter, dataDir, tt.initialKeys, nil, collectTimeout, 0)

			// Simulate that data has been collected by populating the cache
			manager.cachedSystemInfo = &v1beta1.DeviceSystemInfo{}

			cfg := &config.Config{
				SystemInfo: tt.newKeys,
			}

			err = manager.ReloadConfig(context.Background(), cfg)
			require.NoError(err)

			require.Equal(tt.expected, manager.cachedSystemInfo != nil)

			require.Equal(tt.newKeys, manager.infoKeys, "info keys should be updated")
		})
	}
}
