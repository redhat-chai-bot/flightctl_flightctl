package auxiliary

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInsecureRegistryUpdates(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "registries.conf.d", "flightctl-e2e.conf")
	for _, registry := range []string{"old.example:5000", "new.example:5000", "new.example:5000"} {
		require.NoError(t, writeInsecureRegistry(root, registry))
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(content), `location = "`+registry+`"`)
		require.Contains(t, string(content), "insecure = true")
	}
}
