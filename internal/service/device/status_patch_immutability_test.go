package device

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/service/common"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

// deviceFromStoredSpec builds the device that applyDeviceStatusPatch sees after a store read:
// the spec is decoded from the exact bytes the database handed back, which is what gives the
// generated oneOf types (ResourceMonitor and friends) their raw json.RawMessage payload.
func deviceFromStoredSpec(t *testing.T, storedSpecJSON string) *domain.Device {
	t.Helper()
	var spec domain.DeviceSpec
	decoder := json.NewDecoder(strings.NewReader(storedSpecJSON))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&spec))

	status := domain.NewDeviceStatus()
	return &domain.Device{
		ApiVersion: "flightctl.io/v1beta1",
		Kind:       domain.DeviceKind,
		Metadata:   domain.ObjectMeta{Name: lo.ToPtr("foo"), Labels: &map[string]string{"fleet": "none"}},
		Spec:       &spec,
		Status:     &status,
	}
}

// statusOnlyPatch is the patch an agent sends when it reports resource usage and the derived
// summary: it addresses nothing outside /status.
func statusOnlyPatch() domain.PatchRequest {
	var resources interface{} = map[string]interface{}{
		"cpu":    string(domain.DeviceResourceStatusHealthy),
		"disk":   string(domain.DeviceResourceStatusWarning),
		"memory": string(domain.DeviceResourceStatusHealthy),
	}
	var summary interface{} = map[string]interface{}{
		"status": string(domain.DeviceSummaryStatusDegraded),
		"info":   "disk pressure",
	}
	return domain.PatchRequest{
		{Op: "replace", Path: "/status/resources", Value: &resources},
		{Op: "replace", Path: "/status/summary", Value: &summary},
	}
}

// canonicalDiskMonitorSpec is a spec whose bytes already match what encoding/json would emit,
// so it round trips byte-for-byte. It is the control case for the two specs below.
const canonicalDiskMonitorSpec = `{"os":{"image":"quay.io/redhat/rhde:9.2"},` +
	`"resources":[{"monitorType":"Disk","path":"/var","alertRules":[{"severity":"Warning",` +
	`"percentage":75,"duration":"10m","description":"disk usage is high"}],"samplingInterval":"60s"}]}`

// jsonbSpacedDiskMonitorSpec is the same document as written back by a Postgres jsonb column,
// which separates members with ": " and ", ".
const jsonbSpacedDiskMonitorSpec = `{"os": {"image": "quay.io/redhat/rhde:9.2"}, ` +
	`"resources": [{"monitorType": "Disk", "path": "/var", "alertRules": [{"severity": "Warning", ` +
	`"percentage": 75, "duration": "10m", "description": "disk usage is high"}], "samplingInterval": "60s"}]}`

// htmlCharDiskMonitorSpec is the same document with characters that encoding/json escapes as
// <, > and & when it compacts a json.RawMessage.
const htmlCharDiskMonitorSpec = `{"os":{"image":"quay.io/redhat/rhde:9.2"},` +
	`"resources":[{"monitorType":"Disk","path":"/var","alertRules":[{"severity":"Warning",` +
	`"percentage":75,"duration":"10m","description":"free space < 25% & falling"}],"samplingInterval":"60s"}]}`

// TestApplyDeviceStatusPatchSpecImmutability covers the regression where a status-only PATCH
// was rejected with "spec is immutable". DeviceSpec.Resources holds ResourceMonitor values,
// and ResourceMonitor keeps its payload in an unexported json.RawMessage. The round trip
// ApplyJSONPatch performs re-serializes that payload, so a spec whose stored bytes are not
// already in encoding/json's canonical form comes back byte-different - and the old
// reflect.DeepEqual immutability check read that as a spec mutation.
func TestApplyDeviceStatusPatchSpecImmutability(t *testing.T) {
	tests := []struct {
		name         string
		storedSpec   string
		rawRoundTrip bool // whether the raw union bytes survive the patch round trip unchanged
	}{
		{
			name:         "When the stored spec is already canonical JSON it should accept a status-only patch",
			storedSpec:   canonicalDiskMonitorSpec,
			rawRoundTrip: true,
		},
		{
			name:         "When the stored spec carries Postgres jsonb spacing it should accept a status-only patch",
			storedSpec:   jsonbSpacedDiskMonitorSpec,
			rawRoundTrip: false,
		},
		{
			name:         "When the stored spec contains characters encoding/json escapes it should accept a status-only patch",
			storedSpec:   htmlCharDiskMonitorSpec,
			rawRoundTrip: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := deviceFromStoredSpec(t, tt.storedSpec)

			patched, err := applyDeviceStatusPatch(context.Background(), current, statusOnlyPatch(), "foo")
			require.NoError(t, err)
			require.NotNil(t, patched)

			// The patch landed.
			require.Equal(t, domain.DeviceResourceStatusWarning, patched.Status.Resources.Disk)
			require.Equal(t, domain.DeviceSummaryStatusDegraded, patched.Status.Summary.Status)

			// The spec is still the same document.
			equal, err := common.EqualJSON(current.Spec, patched.Spec)
			require.NoError(t, err)
			require.True(t, equal, "status patch must not change the spec document")

			// Pin the root cause: for the two non-canonical specs the decoded Go values differ
			// even though the document does not, which is exactly what the old
			// reflect.DeepEqual check tripped over. If this ever stops holding, the
			// regression these cases guard has moved and the test needs revisiting.
			require.Equal(t, tt.rawRoundTrip, reflect.DeepEqual(current.Spec, patched.Spec),
				"raw json.RawMessage round-trip fidelity changed for this spec")
		})
	}
}

// TestApplyDeviceStatusPatchStillRejectsRealMutations makes sure the relaxed comparison did not
// open the status endpoint up to editing anything outside /status.
func TestApplyDeviceStatusPatchStillRejectsRealMutations(t *testing.T) {
	tests := []struct {
		name    string
		patch   func() domain.PatchRequest
		wantErr string
	}{
		{
			name: "When the patch changes the os image it should reject the spec as immutable",
			patch: func() domain.PatchRequest {
				var value interface{} = "quay.io/redhat/rhde:9.3"
				return domain.PatchRequest{{Op: "replace", Path: "/spec/os/image", Value: &value}}
			},
			wantErr: "spec is immutable",
		},
		{
			name: "When the patch changes a resource monitor path it should reject the spec as immutable",
			patch: func() domain.PatchRequest {
				var value interface{} = "/srv"
				return domain.PatchRequest{{Op: "replace", Path: "/spec/resources/0/path", Value: &value}}
			},
			wantErr: "spec is immutable",
		},
		{
			name: "When the patch removes a resource monitor it should reject the spec as immutable",
			patch: func() domain.PatchRequest {
				return domain.PatchRequest{{Op: "remove", Path: "/spec/resources/0"}}
			},
			wantErr: "spec is immutable",
		},
		{
			name: "When the patch changes a label it should reject the metadata as immutable",
			patch: func() domain.PatchRequest {
				var value interface{} = "prod"
				return domain.PatchRequest{{Op: "replace", Path: "/metadata/labels/fleet", Value: &value}}
			},
			wantErr: "metadata is immutable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use the jsonb-spaced spec so these cases also prove the fix did not simply
			// stop comparing: the comparison still has to flag genuine edits.
			current := deviceFromStoredSpec(t, jsonbSpacedDiskMonitorSpec)

			_, err := applyDeviceStatusPatch(context.Background(), current, tt.patch(), "foo")
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
