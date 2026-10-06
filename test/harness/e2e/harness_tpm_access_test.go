package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostHasTPMDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device")
	require.False(t, HostHasTPMDevice(path))
	require.NoError(t, os.WriteFile(path, nil, 0600))
	require.True(t, HostHasTPMDevice(path))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer file.Close()
	require.True(t, HostHasTPMDevice(path))
}
