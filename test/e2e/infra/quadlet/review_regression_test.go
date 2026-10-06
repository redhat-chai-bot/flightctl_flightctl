package quadlet

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoteRootlessPaths(t *testing.T) {
	t.Setenv("E2E_SSH_HOST", "remote.example")
	t.Setenv("E2E_SSH_USER", "developer")
	t.Setenv("E2E_CONFIG_DIR", "")
	t.Setenv("QUADLET_FILES_OUTPUT_DIR", "")
	_, err := NewInfraProvider("", "", false)
	require.ErrorContains(t, err, "remote rootless Quadlets require explicit")
	t.Setenv("E2E_CONFIG_DIR", "/remote/config/flightctl")
	t.Setenv("QUADLET_FILES_OUTPUT_DIR", "/remote/config/containers/systemd")
	provider, err := NewInfraProvider("", "", false)
	require.NoError(t, err)
	require.Equal(t, "/remote/config/flightctl", provider.configDir)
	require.Equal(t, "/remote/config/flightctl/secrets", provider.secretDir)
	require.Equal(t, "/remote/config/containers/systemd", provider.unitDir)
}

func TestRegistryFilesHaveGeneratedMarkers(t *testing.T) {
	root := t.TempDir()
	provider := &InfraProvider{host: "localhost", unitDir: root}
	ctx := context.Background()
	require.NoError(t, provider.writeRegistryCertMount(ctx, "flightctl-worker.container", "/tmp/certs", "registry.example"))
	dir := filepath.Join(root, "registries.conf.d")
	require.NoError(t, provider.writeRegistriesDir(ctx, dir, "remap", "insecure"))
	for _, path := range []string{
		filepath.Join(root, "flightctl-worker.container.d", registryCertDropInName),
		filepath.Join(dir, "flightctl-remap.conf"),
		filepath.Join(dir, "flightctl-e2e.conf"),
	} {
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(content), generatedRegistryMarker)
	}
}
