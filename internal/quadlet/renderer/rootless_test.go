package renderer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnitScopeAndAuthority(t *testing.T) {
	for _, test := range []struct {
		name, dir, port, suffix string
		user                    bool
	}{
		{"When rendering user units it should retain port 9443", "/home/test/.config/systemd/user", "9443", ":9443", true},
		{"When rendering system units it should use port 443", "/usr/lib/systemd/system", "443", "", false},
		{"When staging RPM units it should use system scope", "/buildroot/usr/lib/systemd/system", "443", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := &RendererConfig{SystemdUnitOutputDir: test.dir}
			config.selectUnitScope()
			require.Equal(t, test.user, config.UserScope)
			require.Equal(t, test.port, config.GatewayHostPort)
			require.Equal(t, test.suffix, config.GatewayHostPortSuffix)
			actions := servicesManifest(config)
			nested := false
			for _, action := range actions {
				if strings.HasSuffix(action.Source, "10-rootless-kvm.conf") {
					nested = true
				}
				if strings.Contains(action.Source, "state.conf") {
					require.Equal(t, test.user, strings.Contains(action.Source, "99-rootless"))
					if !test.user {
						require.True(t, action.Template)
					}
				}
			}
			require.Equal(t, test.user, nested)
		})
	}
}

func TestStaticMountLabelsAndServiceTargets(t *testing.T) {
	for _, userScope := range []bool{false, true} {
		for _, service := range []string{"db", "kv", "grafana", "prometheus", "userinfo-proxy", "telemetry-gateway"} {
			t.Run(service+" userScope="+fmt.Sprint(userScope), func(t *testing.T) {
				config := createTestConfig(t)
				config.UserScope = userScope
				config.Kv.Image = "redis"
				name := "flightctl-" + service + ".container"
				destination := filepath.Join(t.TempDir(), name)
				require.NoError(t, processFile(filepath.Join("../../../deploy/podman", "flightctl-"+service, name), destination, true, RegularFileMode, config))
				content, err := os.ReadFile(destination)
				require.NoError(t, err)
				if service == "db" || service == "kv" {
					for _, line := range strings.Split(string(content), "\n") {
						if strings.HasPrefix(line, "Volume=%D/") {
							require.Equal(t, userScope, strings.HasSuffix(line, ",z"))
						}
					}
				} else {
					require.NotContains(t, string(content), "WantedBy=multi-user.target")
					require.Contains(t, string(content), "WantedBy=flightctl")
				}
			})
		}
	}
}

func TestSystemGrafanaOwnershipDropin(t *testing.T) {
	directory := t.TempDir()
	config := &RendererConfig{QuadletFilesOutputDir: directory}
	config.Grafana.Image = "docker.io/grafana/grafana"
	config.Grafana.Tag = "latest"
	source := "../../../deploy/podman/flightctl-grafana/flightctl-grafana.container.d/98-system-state.conf"
	destination := filepath.Join(directory, "flightctl-grafana.container.d", "98-system-state.conf")
	require.NoError(t, processFile(source, destination, true, 0644, config))
	data, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Contains(t, string(data), "podman image inspect docker.io/grafana/grafana:latest")
	require.Contains(t, string(data), "chown -R")
	require.Contains(t, string(data), "%S/grafana || true")
	require.NotContains(t, string(data), "{{")
}

func TestSpecifierPathsAndStaging(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for key, value := range map[string]string{"XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_DATA_HOME": filepath.Join(home, "data"), "XDG_STATE_HOME": filepath.Join(home, "state")} {
		t.Setenv(key, value)
	}
	config := &RendererConfig{UserScope: true, WriteableConfigOutputDir: filepath.Join(home, "config/flightctl"), ReadOnlyConfigOutputDir: filepath.Join(home, "data/flightctl"), BinOutputDir: filepath.Join(home, "data/flightctl/bin"), QuadletFilesOutputDir: filepath.Join(home, "config/containers/systemd"), SystemdUnitOutputDir: filepath.Join(home, "config/systemd/user"), VarTmpOutputDir: filepath.Join(home, "tmp"), VarLibOutputDir: filepath.Join(home, "state")}
	require.NoError(t, config.validateSpecifierPaths())
	config.BinOutputDir = "/other"
	require.Error(t, config.validateSpecifierPaths())
	require.Equal(t, "/buildroot", systemdBuildroot("/buildroot/usr/lib/systemd/system"))
	require.Equal(t, "/buildroot/var/tmp", systemPath("/buildroot", "var", "tmp"))
	require.Equal(t, "/var/tmp", unstageSystemPath("/buildroot/var/tmp", "/buildroot"))
	require.Equal(t, "/var/tmp", buildStorageHostDir(&RendererConfig{SystemdUnitOutputDir: "/buildroot/usr/lib/systemd/system", VarTmpOutputDir: "/buildroot/var/tmp"}))
	require.Error(t, (&RendererConfig{SystemdUnitOutputDir: "/buildroot/usr/lib/systemd/system", VarTmpOutputDir: "/outside"}).validateSpecifierPaths())
	require.Error(t, requireOutputPath("test", "/wrong", "/expected"))
}

func TestGeneratedUnitsAndSymlinkReplacement(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "systemd")
	file := filepath.Join(directory, "flightctl.target")
	require.NoError(t, writeRenderedFile(file, "[Unit]\n", 0600, &RendererConfig{SystemdUnitOutputDir: directory}))
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Contains(t, string(data), generatedMarker)
	link := filepath.Join(directory, "default.target.wants/flightctl.target")
	require.NoError(t, createSymlink("../old.target", link))
	require.NoError(t, createSymlink("../flightctl.target", link))
	target, err := os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, "../flightctl.target", target)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "regular"), []byte("keep"), 0600))
	require.ErrorContains(t, createSymlink("elsewhere", filepath.Join(directory, "regular")), "not a replaceable symlink")
}

func TestUnitFileClassification(t *testing.T) {
	root := filepath.Join(t.TempDir(), "systemd-stuff")
	config := &RendererConfig{QuadletFilesOutputDir: filepath.Join(root, "quadlets"), SystemdUnitOutputDir: filepath.Join(root, "units")}
	for _, test := range []struct {
		path string
		unit bool
	}{
		{"quadlets/flightctl.container", true},
		{"quadlets/flightctl.container.d/10-port.conf", true},
		{"units/flightctl.target", true},
		{"units/flightctl.service", true},
		{"data/redis.conf", false},
		{"data/valkey.conf", false},
		{"units-other/flightctl.service", false},
		{"quadlets/config.yaml", false},
	} {
		t.Run("When rendering "+test.path+" it should classify only configured unit directories", func(t *testing.T) {
			path := filepath.Join(root, test.path)
			require.Equal(t, test.unit, isUnitFile(path, config))
			require.NoError(t, writeRenderedFile(path, "content\n", 0600, config))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, test.unit, strings.HasPrefix(string(data), generatedMarker))
		})
	}
	require.False(t, isUnitFile(filepath.Join(root, "flightctl.service"), &RendererConfig{}))
}
