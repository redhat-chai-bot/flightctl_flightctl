package vmrender_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/tasks"
	flightlog "github.com/flightctl/flightctl/pkg/log"
	containers "github.com/flightctl/flightctl/test/harness/containers"
	"github.com/flightctl/flightctl/test/integration/integrationstack"
	testutil "github.com/flightctl/flightctl/test/util"
	"github.com/flightctl/flightctl/test/util/testdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
)

const (
	vmToQuadletImage = "quay.io/flightctl/vm-to-quadlet:2d2a64d59c10bbe33943e1c74835e5063177c3fb"
)

var (
	suiteCtx      context.Context
	redisHost     string
	redisPort     uint
	redisPassword domain.SecureString
	redisCleanup  func()

	vmConverter     tasks.VmConverterFn
	vmBinaryPath    string
	vmBinaryCleanup func()
)

func TestVmRender(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VmRender Suite")
}

// SynchronizedBeforeSuite ensures the expensive binary extraction and Redis
// container startup run only once (on proc 1). The resulting paths and
// connection info are broadcast as JSON to all procs, which each initialise
// their own vmConverter, Redis connection vars, and tracer.
var _ = SynchronizedBeforeSuite(
	// Proc 1 only: extract binary and start a single shared Redis container.
	func(ctx context.Context) []byte {
		Expect(integrationstack.EnsureRunning(ctx)).To(Succeed())
		binaryPath, cleanup, err := extractVmToQuadletBinary(ctx)
		Expect(err).ToNot(HaveOccurred(), "failed to extract vm-to-quadlet binary")
		vmBinaryCleanup = cleanup

		host, port, password, rCleanup, err := testdb.CreateTestRedis(ctx, flightlog.InitLogs())
		Expect(err).ToNot(HaveOccurred())
		redisCleanup = rCleanup

		info, err := json.Marshal(map[string]interface{}{
			"binaryPath": binaryPath,
			"host":       host, "port": port, "password": string(password),
		})
		Expect(err).ToNot(HaveOccurred())
		return info
	},
	// All procs: receive shared binary path and Redis connection; set up per-process state.
	func(ctx context.Context, data []byte) {
		suiteCtx = testutil.InitSuiteTracerForGinkgo("VmRender Suite")
		Expect(integrationstack.EnsureRunning(suiteCtx)).To(Succeed())

		var info map[string]interface{}
		Expect(json.Unmarshal(data, &info)).To(Succeed())
		redisHost = info["host"].(string)
		redisPort = uint(info["port"].(float64))
		redisPassword = domain.SecureString(info["password"].(string))

		vmBinaryPath = info["binaryPath"].(string)
		vmConverter = tasks.NewVmConverter(vmBinaryPath, tasks.DefaultVmRenderOptions())
	},
)

// SynchronizedAfterSuite mirrors the above: per-process cleanup first, then
// proc-1 teardown (Redis container + binary temp dir) last.
var _ = SynchronizedAfterSuite(
	func() {
		// All procs: no per-process cleanup needed (Redis is shared).
	},
	func() {
		// Proc 1 only: tear down shared Redis container and binary temp dir.
		if redisCleanup != nil {
			redisCleanup()
		}
		if vmBinaryCleanup != nil {
			vmBinaryCleanup()
		}
	},
)

// extractVmToQuadletBinary pulls the pre-built vm-to-quadlet image and copies
// the binary out via CopyFileFromContainer into a temp directory. The container
// is terminated immediately after extraction. Returns the absolute binary path
// and a cleanup func.
func extractVmToQuadletBinary(ctx context.Context) (string, func(), error) {
	containers.ConfigureDockerHost()

	req := testcontainers.ContainerRequest{
		Image: vmToQuadletImage,
		// Keep the container alive so CopyFileFromContainer can read from it.
		Cmd: []string{"/bin/sh", "-c", "sleep infinity"},
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ProviderType:     containers.GetProviderType(),
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return "", nil, fmt.Errorf("start vm-to-quadlet container: %w", err)
	}
	cleanup := func() {
		termCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(termCtx)
	}

	rc, err := c.CopyFileFromContainer(ctx, "/usr/local/bin/vm-to-quadlet")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copy binary from container: %w", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("read binary: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "flightctl-vm-to-quadlet-*")
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create temp dir: %w", err)
	}
	combinedCleanup := func() {
		cleanup()
		_ = os.RemoveAll(tmpDir)
	}

	binaryPath := filepath.Join(tmpDir, "vm-to-quadlet")
	if err := os.WriteFile(binaryPath, data, 0700); err != nil { //nolint:gosec // executable binary requires execute permission
		combinedCleanup()
		return "", nil, fmt.Errorf("write binary: %w", err)
	}
	return binaryPath, combinedCleanup, nil
}
