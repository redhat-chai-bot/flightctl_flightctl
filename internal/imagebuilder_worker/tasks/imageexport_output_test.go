package tasks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/flightctl/flightctl/internal/imagebuilder_api/domain"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestNativeImageExportOutput(t *testing.T) {
	for _, test := range []struct {
		format domain.ExportFormatType
		path   string
	}{
		{domain.ExportFormatTypeISO, "iso/disk.iso"},
		{domain.ExportFormatTypeQCOW2, "qcow2/disk.qcow2"},
		{domain.ExportFormatTypeVMDK, "vmdk/disk.vmdk"},
		{domain.ExportFormatTypeQCOW2DiskContainer, "qcow2/disk.qcow2"},
	} {
		t.Run("When exporting "+string(test.format)+" it should locate the native CLI output", func(t *testing.T) {
			outputDir := t.TempDir()
			consumer := &Consumer{}
			_, err := consumer.findOutputFile(outputDir, test.format, logrus.New())
			require.ErrorContains(t, err, "output file not found")
			expected := filepath.Join(outputDir, test.path)
			require.NoError(t, os.MkdirAll(filepath.Dir(expected), 0700))
			require.NoError(t, os.WriteFile(expected, []byte("disk"), 0600))
			actual, err := consumer.findOutputFile(outputDir, test.format, logrus.New())
			require.NoError(t, err)
			require.Equal(t, expected, actual)
			args := nativeImageBuilderArgs("builder", "registry.example/os:latest", test.format)
			require.Equal(t, []string{"exec", "builder", "/tmp/image-builder", "build", "--in-vm", "--bootc-ref", "registry.example/os:latest", "--bootc-default-fs", "xfs", "--output-dir", filepath.Join("/output", filepath.Dir(test.path)), "--output-name", "disk", string(nativeExportFormat(test.format))}, args)
		})
	}
}
