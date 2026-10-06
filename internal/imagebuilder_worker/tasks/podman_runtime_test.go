package tasks

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestRootlessUIDMap(t *testing.T) {
	for _, test := range []struct {
		name    string
		mapping string
		err     error
		want    bool
	}{
		{"When UID maps the host it should use rootful", "0 0 4294967295", nil, false},
		{"When UID maps a user it should use rootless", "0 1000 1", nil, true},
		{"When mapping is empty it should fail closed", "", nil, true},
		{"When mapping is malformed it should fail closed", "garbage", nil, true},
		{"When mapping has multiple ranges it should use rootless", "0 0 4294967295\n1 1000 1", nil, true},
		{"When proc is unreadable it should fail closed", "", errors.New("unreadable"), true},
	} {
		t.Run(test.name, func(t *testing.T) { require.Equal(t, test.want, rootlessFromUIDMap([]byte(test.mapping), test.err)) })
	}
}

func TestPodmanRuntimeEnvironment(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/inherited")
	t.Setenv("XDG_RUNTIME_DIR", "/inherited-runtime")
	for _, rootless := range []bool{false, true} {
		t.Run(map[bool]string{false: "When rootful it should preserve runtime", true: "When rootless it should isolate runtime"}[rootless], func(t *testing.T) {
			worker := &podmanWorker{Rootless: rootless, TmpDir: "/job/build", TmpOutDir: "/job/out", StorageConfigPath: "/job/storage.conf", PodmanRuntimeDir: "/job/runtime", AuthFilePath: "/job/auth.json"}
			command := worker.podmanCommand(context.Background(), "info")
			if rootless {
				require.Contains(t, command.Env, "XDG_DATA_HOME=/var/lib")
				require.Contains(t, command.Env, "XDG_RUNTIME_DIR=/job/runtime")
				require.Contains(t, command.Env, "CONTAINERS_STORAGE_CONF=/job/storage.conf")
				require.NotContains(t, command.Env, "XDG_DATA_HOME=/inherited")
				require.Equal(t, "/job/auth.json", worker.authFilePath())
				require.Equal(t, "/job/build/file", worker.workerContainerPath("/build/file"))
				require.Equal(t, "/job/out/file", worker.workerContainerPath("/output/file"))
			} else {
				require.Nil(t, command.Env)
				require.Equal(t, containerAuthFile, worker.authFilePath())
				require.Equal(t, "/build/file", worker.workerContainerPath("/build/file"))
			}
			require.Equal(t, "/other", worker.workerContainerPath("/other"))
			require.Equal(t, worker.authFilePath(), worker.authFileEnvironment()["REGISTRY_AUTH_FILE"])
		})
	}
	values := mergeCommandEnv(map[string]string{"XDG_DATA_HOME": "/override"})
	count := 0
	for _, value := range values {
		if strings.HasPrefix(value, "XDG_DATA_HOME=") {
			count++
		}
	}
	require.Equal(t, 1, count)
	require.Equal(t, "/inherited", os.Getenv("XDG_DATA_HOME"))
}

func TestRegistryCertificateDirectories(t *testing.T) {
	worker := &podmanWorker{Rootless: true, TmpAuthDir: t.TempDir()}
	for _, registry := range []string{"source.example:5000", "destination.example:5001"} {
		t.Run("When registry is "+registry+" it should retain the CA directory", func(t *testing.T) {
			encoded := base64.StdEncoding.EncodeToString([]byte("test certificate"))
			require.NoError(t, worker.installCACert(context.Background(), &encoded, registry, logrus.New()))
			directory := filepath.Join(worker.TmpAuthDir, "certs", registry)
			require.Equal(t, directory, worker.registryCertDir(registry))
			data, err := os.ReadFile(filepath.Join(directory, "ca.crt"))
			require.NoError(t, err)
			require.Equal(t, "test certificate", string(data))
		})
	}
	require.NotEqual(t, worker.registryCertDir("source.example:5000"), worker.registryCertDir("destination.example:5001"))
}
