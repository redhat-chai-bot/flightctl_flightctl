package v1alpha1

import (
	"strconv"

	"github.com/flightctl/flightctl/api/core/v1beta1"
)

// DeviceFeatureName is the name of a device feature that a catalog item version
// can require of a device. Only the values enumerated below are valid keys in a
// CatalogItemVersion's deviceFeatures map.
type DeviceFeatureName string

const (
	// DeviceFeatureGPUPresent indicates a GPU is present and available on the device.
	DeviceFeatureGPUPresent DeviceFeatureName = "gpu.present"
	// DeviceFeatureKVMEnabled indicates KVM kernel modules are loaded and /dev/kvm is present.
	DeviceFeatureKVMEnabled DeviceFeatureName = "kvm.enabled"
	// DeviceFeatureOSMode indicates whether the device uses bootc image mode or
	// traditional package mode.
	DeviceFeatureOSMode DeviceFeatureName = "os.mode"
)

// ValidDeviceFeatureValues maps each valid device feature name to the set of
// values accepted for that feature. Boolean-style features reuse the canonical
// strconv boolean strings ("true"/"false"); the os.mode feature reuses the
// existing v1beta1.OsMode* values ("image"/"package").
var ValidDeviceFeatureValues = map[DeviceFeatureName][]string{
	DeviceFeatureGPUPresent: {strconv.FormatBool(true), strconv.FormatBool(false)},
	DeviceFeatureKVMEnabled: {strconv.FormatBool(true), strconv.FormatBool(false)},
	DeviceFeatureOSMode:     {string(v1beta1.OsModeImage), string(v1beta1.OsModePackage)},
}

func (s *CatalogItemSpec) FindVersion(version string) *CatalogItemVersion {
	for i := range s.Versions {
		if s.Versions[i].Version == version {
			return &s.Versions[i]
		}
	}
	return nil
}

func (s *CatalogItemSpec) FindArtifact(artifactType CatalogItemArtifactType) *CatalogItemArtifact {
	for i := range s.Artifacts {
		if s.Artifacts[i].Type == artifactType {
			return &s.Artifacts[i]
		}
	}
	return nil
}
