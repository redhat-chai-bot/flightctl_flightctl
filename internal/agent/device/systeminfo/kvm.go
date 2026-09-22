package systeminfo

import (
	"github.com/flightctl/flightctl/internal/agent/device/fileio"
	"github.com/flightctl/flightctl/pkg/log"
)

// devKVMPath is the KVM character device. Its presence and accessibility
// indicate that KVM hardware virtualization is active and available on the host.
const devKVMPath = "/dev/kvm"

// collectKVMInfo reports whether KVM hardware virtualization is available by
// checking that the /dev/kvm device node exists and can be opened. Detection is
// best effort: any error accessing the device is treated as KVM being
// unavailable.
func collectKVMInfo(log *log.PrefixLogger, reader fileio.Reader) KVMInfo {
	// WithSkipContentCheck confirms the device node exists and can be opened
	// (i.e. is accessible) without attempting to read bytes from it, which is
	// required for a character device such as /dev/kvm.
	enabled, err := reader.PathExists(devKVMPath, fileio.WithSkipContentCheck())
	if err != nil {
		log.Debugf("Unable to determine KVM availability from %s: %v", devKVMPath, err)
		return KVMInfo{Enabled: false}
	}
	return KVMInfo{Enabled: enabled}
}
