package tasks_test

import (
	"context"
	"path/filepath"
	"runtime"

	api "github.com/flightctl/flightctl/api/core/v1beta1"
	"github.com/flightctl/flightctl/internal/config"
	"github.com/flightctl/flightctl/internal/consts"
	"github.com/flightctl/flightctl/internal/domain"
	"github.com/flightctl/flightctl/internal/kvstore"
	deviceservice "github.com/flightctl/flightctl/internal/service/device"
	"github.com/flightctl/flightctl/internal/service/events"
	labelsyncmappingservice "github.com/flightctl/flightctl/internal/service/labelsyncmapping"
	"github.com/flightctl/flightctl/internal/store"
	devicestore "github.com/flightctl/flightctl/internal/store/device"
	eventstore "github.com/flightctl/flightctl/internal/store/event"
	labelsyncmappingstore "github.com/flightctl/flightctl/internal/store/labelsyncmapping"
	"github.com/flightctl/flightctl/internal/tasks"
	"github.com/flightctl/flightctl/internal/util"
	"github.com/flightctl/flightctl/internal/worker_client"
	flightlog "github.com/flightctl/flightctl/pkg/log"
	"github.com/flightctl/flightctl/pkg/queues"
	testutil "github.com/flightctl/flightctl/test/util"
	"github.com/flightctl/flightctl/test/util/testdb"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	"github.com/sirupsen/logrus"
	"go.uber.org/mock/gomock"
	"gorm.io/gorm"
)

// Feature labels derived by the mappings shipped in
// packaging/flightctl/label-sync/mappings.yaml.
const (
	gpuPresentLabel  = "feature.flightctl.io/gpu.present"
	kvmEnabledLabel  = "feature.flightctl.io/kvm.enabled"
	osModeLabel      = "feature.flightctl.io/os.mode"
	architectureInfo = "feature.flightctl.io/systemInfo.architecture"
	agentVersionInfo = "feature.flightctl.io/systemInfo.agentVersion"
	distroIDInfo     = "feature.flightctl.io/systemInfo.distroId"
	siteCustomInfo   = "feature.flightctl.io/customInfo.site"
)

// packagedMappingsPath resolves the default mapping manifest that ships with the
// product, so these tests exercise the real expressions rather than a copy.
func packagedMappingsPath() string {
	_, sourceFile, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue(), "could not locate the test source file")
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../.."))
	return filepath.Join(repoRoot, "packaging/flightctl/label-sync/mappings.yaml")
}

var _ = Describe("Device capability feature labels", func() {
	var (
		ctx          context.Context
		log          *logrus.Logger
		orgID        uuid.UUID
		cfg          *config.Config
		dbName       string
		db           *gorm.DB
		deviceStore  devicestore.Store
		mappingStore labelsyncmappingstore.Store
		deviceSvc    deviceservice.Service
		mappingSvc   labelsyncmappingservice.Service
		ctrl         *gomock.Controller
		kvStoreInst  kvstore.KVStore
	)

	BeforeEach(func() {
		ctx = testutil.StartSpecTracerForGinkgo(suiteCtx)
		ctx = context.WithValue(ctx, consts.EventSourceComponentCtxKey, "flightctl-worker")
		ctx = context.WithValue(ctx, consts.EventActorCtxKey, "service:flightctl-worker")
		orgID = store.NullOrgId
		log = flightlog.InitLogs()

		var err error
		cfg, dbName, db, err = testdb.CreateTestDB(ctx, log, "", store.InitDB)
		Expect(err).NotTo(HaveOccurred())

		deviceStore = devicestore.NewDeviceStore(db, log.WithField("pkg", "device-store"))
		mappingStore = labelsyncmappingstore.NewStore(db, log.WithField("pkg", "labelsyncmapping-store"))
		eventStore := eventstore.NewEventStore(db, log.WithField("pkg", "event-store"))

		ctrl = gomock.NewController(GinkgoT())
		producer := queues.NewMockQueueProducer(ctrl)
		producer.EXPECT().Enqueue(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
		eventsSvc := events.NewServiceHandler(eventStore, worker_client.NewWorkerClient(producer, log), log)

		kvStoreInst, err = kvstore.NewKVStore(ctx, log, redisHost, redisPort, redisPassword)
		Expect(err).NotTo(HaveOccurred())
		deviceSvc = deviceservice.NewDeviceServiceHandler(deviceStore, nil, nil, eventsSvc, kvStoreInst, "", log)

		mappingService, err := labelsyncmappingservice.NewServiceHandler(mappingStore, deviceStore, mustNewEvaluator(), eventsSvc, log)
		Expect(err).NotTo(HaveOccurred())
		mappingSvc = mappingService
	})

	AfterEach(func() {
		if kvStoreInst != nil {
			kvStoreInst.Close()
		}
		Expect(testdb.DeleteTestDB(ctx, log, cfg, db, dbName)).To(Succeed())
		ctrl.Finish()
	})

	// provisionDefaultMappings seeds the organization exactly the way the server
	// seeds a newly created organization at runtime.
	provisionDefaultMappings := func() {
		provisioner, err := labelsyncmappingservice.NewInitialLabelSyncMappingProvisioner(packagedMappingsPath(), mappingSvc, log)
		Expect(err).NotTo(HaveOccurred())
		provisioner.Provision(ctx, orgID)
	}

	createDevice := func(name string, systemInfo api.DeviceSystemInfo, labels map[string]string) *domain.Device {
		deviceStatus := domain.NewDeviceStatus()
		deviceStatus.SystemInfo = systemInfo
		device := domain.Device{
			ApiVersion: domain.DeviceAPIVersion,
			Kind:       domain.DeviceKind,
			Metadata: domain.ObjectMeta{
				Name:   lo.ToPtr(name),
				Labels: &labels,
			},
			Spec:   &domain.DeviceSpec{},
			Status: &deviceStatus,
		}
		created, status := deviceSvc.CreateDevice(ctx, orgID, device)
		Expect(status.Code).To(Equal(int32(201)), status.Message)
		return created
	}

	// reconcile runs the same worker logic the label-sync worker runs for a device event.
	reconcile := func(name string) {
		logic, err := tasks.NewDeviceLabelReconciliationLogic(log, mappingSvc, orgID, domain.Event{
			Reason: domain.EventReasonResourceUpdated,
			InvolvedObject: domain.ObjectReference{
				Kind: domain.DeviceKind,
				Name: name,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(logic.Reconcile(ctx)).To(Succeed())
	}

	deviceLabels := func(name string) map[string]string {
		persisted, err := deviceStore.Get(ctx, orgID, name)
		Expect(err).NotTo(HaveOccurred())
		return lo.FromPtr(persisted.Metadata.Labels)
	}

	// listDeviceNames returns the device names matching a label selector, so the
	// derived labels are exercised through the same selector path operators use.
	listDeviceNames := func(labelSelector string) []string {
		list, status := deviceSvc.ListDevices(ctx, orgID, domain.ListDevicesParams{
			LabelSelector: lo.ToPtr(labelSelector),
		}, nil)
		Expect(status.Code).To(Equal(int32(200)), status.Message)
		names := make([]string, 0, len(list.Items))
		for _, item := range list.Items {
			names = append(names, lo.FromPtr(item.Metadata.Name))
		}
		return names
	}

	reportingSystemInfo := func() api.DeviceSystemInfo {
		return api.DeviceSystemInfo{
			AgentVersion:    "1.4.0",
			Architecture:    "amd64",
			BootID:          "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
			OperatingSystem: "linux",
			AdditionalProperties: map[string]string{
				"distroId":      "rhel",
				"distroVersion": "9.4",
			},
		}
	}

	Context("Default mappings provisioning", func() {
		It("When an organization is provisioned it should install every packaged capability mapping", func() {
			provisionDefaultMappings()

			list, status := mappingSvc.ListLabelSyncMappings(ctx, orgID, domain.ListLabelSyncMappingsParams{})
			Expect(status.Code).To(Equal(int32(200)), status.Message)

			keysByName := make(map[string]*string, len(list.Items))
			for _, mapping := range list.Items {
				Expect(mapping.Spec.ResourceType).To(Equal(domain.LabelSyncMappingDevice))
				keysByName[lo.FromPtr(mapping.Metadata.Name)] = mapping.Spec.Key
			}

			Expect(keysByName).To(HaveKey("system-info"))
			Expect(keysByName).To(HaveKey("custom-info"))
			Expect(keysByName["system-info"]).To(BeNil(), "the systemInfo mapping must emit a label map")
			Expect(keysByName["custom-info"]).To(BeNil(), "the customInfo mapping must emit a label map")
			Expect(keysByName).To(HaveKeyWithValue("gpu-present", lo.ToPtr(gpuPresentLabel)))
			Expect(keysByName).To(HaveKeyWithValue("kvm-enabled", lo.ToPtr(kvmEnabledLabel)))
			Expect(keysByName).To(HaveKeyWithValue("os-mode", lo.ToPtr(osModeLabel)))
		})

		It("When provisioning runs twice it should keep a single copy of each mapping", func() {
			provisionDefaultMappings()
			first, status := mappingSvc.ListLabelSyncMappings(ctx, orgID, domain.ListLabelSyncMappingsParams{})
			Expect(status.Code).To(Equal(int32(200)), status.Message)

			provisionDefaultMappings()
			second, status := mappingSvc.ListLabelSyncMappings(ctx, orgID, domain.ListLabelSyncMappingsParams{})
			Expect(status.Code).To(Equal(int32(200)), status.Message)

			Expect(second.Items).To(HaveLen(len(first.Items)))
		})
	})

	Context("Feature label derivation from reported systemInfo", func() {
		BeforeEach(provisionDefaultMappings)

		It("When a device reports a GPU, KVM, and image OS mode it should derive matching feature labels", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{{Index: 0, Vendor: lo.ToPtr("NVIDIA"), Model: lo.ToPtr("RTX 4090")}}
			systemInfo.Kvm = &api.DeviceKvm{Enabled: lo.ToPtr(true)}
			systemInfo.OsMode = lo.ToPtr(domain.OsModeImage)
			createDevice("capable-device", systemInfo, map[string]string{"site": "east"})

			reconcile("capable-device")

			labels := deviceLabels("capable-device")
			Expect(labels).To(HaveKeyWithValue(gpuPresentLabel, "true"))
			Expect(labels).To(HaveKeyWithValue(kvmEnabledLabel, "true"))
			Expect(labels).To(HaveKeyWithValue(osModeLabel, string(domain.OsModeImage)))
			Expect(labels).To(HaveKeyWithValue(architectureInfo, "amd64"))
			Expect(labels).To(HaveKeyWithValue(agentVersionInfo, "1.4.0"))
			Expect(labels).To(HaveKeyWithValue(distroIDInfo, "rhel"))
			Expect(labels).To(HaveKeyWithValue("site", "east"), "operator labels must survive derivation")
		})

		It("When a device reports no GPU and disabled KVM it should derive explicit false feature labels", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{}
			systemInfo.Kvm = &api.DeviceKvm{Enabled: lo.ToPtr(false)}
			systemInfo.OsMode = lo.ToPtr(domain.OsModePackage)
			createDevice("plain-device", systemInfo, map[string]string{})

			reconcile("plain-device")

			labels := deviceLabels("plain-device")
			Expect(labels).To(HaveKeyWithValue(gpuPresentLabel, "false"))
			Expect(labels).To(HaveKeyWithValue(kvmEnabledLabel, "false"))
			Expect(labels).To(HaveKeyWithValue(osModeLabel, string(domain.OsModePackage)))
		})

		It("When a device never reports a capability it should omit the feature label instead of deriving false", func() {
			createDevice("legacy-device", reportingSystemInfo(), map[string]string{})

			reconcile("legacy-device")

			labels := deviceLabels("legacy-device")
			Expect(labels).NotTo(HaveKey(gpuPresentLabel))
			Expect(labels).NotTo(HaveKey(kvmEnabledLabel))
			Expect(labels).NotTo(HaveKey(osModeLabel))
			Expect(labels).To(HaveKeyWithValue(architectureInfo, "amd64"),
				"fields the legacy agent does report must still derive labels")
		})

		It("When KVM is reported without the enabled field it should omit the KVM feature label", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Kvm = &api.DeviceKvm{}
			createDevice("partial-kvm-device", systemInfo, map[string]string{})

			reconcile("partial-kvm-device")

			Expect(deviceLabels("partial-kvm-device")).NotTo(HaveKey(kvmEnabledLabel))
		})

		It("When a device reports customInfo it should derive namespaced customInfo labels", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.CustomInfo = &api.CustomDeviceInfo{"site": "factory-floor-2"}
			createDevice("custom-info-device", systemInfo, map[string]string{})

			reconcile("custom-info-device")

			Expect(deviceLabels("custom-info-device")).To(HaveKeyWithValue(siteCustomInfo, "factory-floor-2"))
		})

		It("When reported capabilities change it should update the derived feature labels", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{{Index: 0, Vendor: lo.ToPtr("NVIDIA")}}
			systemInfo.Kvm = &api.DeviceKvm{Enabled: lo.ToPtr(true)}
			createDevice("changing-device", systemInfo, map[string]string{})
			reconcile("changing-device")
			Expect(deviceLabels("changing-device")).To(HaveKeyWithValue(gpuPresentLabel, "true"))

			updated := reportingSystemInfo()
			updated.Gpus = &[]api.DeviceGpu{}
			updated.Kvm = &api.DeviceKvm{Enabled: lo.ToPtr(false)}
			info, err := util.StructToMap(updated)
			Expect(err).NotTo(HaveOccurred())
			var value interface{} = info
			_, status := deviceSvc.PatchDeviceStatus(ctx, orgID, "changing-device", domain.PatchRequest{
				{Op: "replace", Path: "/status/systemInfo", Value: &value},
			})
			Expect(status.Code).To(Equal(int32(200)), status.Message)

			reconcile("changing-device")

			labels := deviceLabels("changing-device")
			Expect(labels).To(HaveKeyWithValue(gpuPresentLabel, "false"))
			Expect(labels).To(HaveKeyWithValue(kvmEnabledLabel, "false"))
		})

		It("When a capability stops being reported it should remove the derived feature label", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{{Index: 0}}
			createDevice("silenced-device", systemInfo, map[string]string{})
			reconcile("silenced-device")
			Expect(deviceLabels("silenced-device")).To(HaveKeyWithValue(gpuPresentLabel, "true"))

			info, err := util.StructToMap(reportingSystemInfo())
			Expect(err).NotTo(HaveOccurred())
			var value interface{} = info
			_, status := deviceSvc.PatchDeviceStatus(ctx, orgID, "silenced-device", domain.PatchRequest{
				{Op: "replace", Path: "/status/systemInfo", Value: &value},
			})
			Expect(status.Code).To(Equal(int32(200)), status.Message)

			reconcile("silenced-device")

			Expect(deviceLabels("silenced-device")).NotTo(HaveKey(gpuPresentLabel))
		})

		It("When an operator sets a reserved feature label it should reconcile it to the derived value", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{}
			createDevice("spoofed-device", systemInfo, map[string]string{
				"site":          "main",
				gpuPresentLabel: "true",
			})

			reconcile("spoofed-device")

			labels := deviceLabels("spoofed-device")
			Expect(labels).To(HaveKeyWithValue(gpuPresentLabel, "false"), "the reserved namespace is server owned")
			Expect(labels).To(HaveKeyWithValue("site", "main"), "operator labels outside the namespace are untouched")
		})

		It("When an operator edits an already derived feature label it should reject the change", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{}
			createDevice("protected-device", systemInfo, map[string]string{"site": "main"})
			reconcile("protected-device")
			Expect(deviceLabels("protected-device")).To(HaveKeyWithValue(gpuPresentLabel, "false"))

			var spoofed interface{} = map[string]string{"site": "main", gpuPresentLabel: "true"}
			_, status := deviceSvc.PatchDevice(ctx, orgID, "protected-device", domain.PatchRequest{
				{Op: "replace", Path: "/metadata/labels", Value: &spoofed},
			}, true, true)
			Expect(status.Code).To(Equal(int32(409)), status.Message)

			persisted, err := deviceStore.Get(ctx, orgID, "protected-device")
			Expect(err).NotTo(HaveOccurred())
			replacement := *persisted
			replacementLabels := map[string]string{"site": "main", gpuPresentLabel: "true"}
			replacement.Metadata.Labels = &replacementLabels
			_, status = deviceSvc.ReplaceDevice(ctx, orgID, "protected-device", replacement, nil, true, true)
			Expect(status.Code).To(Equal(int32(409)), status.Message)

			labels := deviceLabels("protected-device")
			Expect(labels).To(HaveKeyWithValue(gpuPresentLabel, "false"))
			Expect(labels).To(HaveKeyWithValue("site", "main"))
		})

		It("When an operator edits labels outside the reserved namespace it should keep the derived labels", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{{Index: 0}}
			createDevice("operator-edit-device", systemInfo, map[string]string{"site": "main"})
			reconcile("operator-edit-device")

			var operatorLabels interface{} = map[string]string{"site": "west", "tier": "edge"}
			_, status := deviceSvc.PatchDevice(ctx, orgID, "operator-edit-device", domain.PatchRequest{
				{Op: "replace", Path: "/metadata/labels", Value: &operatorLabels},
			}, true, true)
			Expect(status.Code).To(Equal(int32(200)), status.Message)

			labels := deviceLabels("operator-edit-device")
			Expect(labels).To(HaveKeyWithValue("site", "west"))
			Expect(labels).To(HaveKeyWithValue("tier", "edge"))
			Expect(labels).To(HaveKeyWithValue(gpuPresentLabel, "true"), "derived labels survive operator label edits")
		})

		It("When devices differ by capability it should return only the matching devices for a feature label selector", func() {
			gpuSystemInfo := reportingSystemInfo()
			gpuSystemInfo.Gpus = &[]api.DeviceGpu{{Index: 0}}
			gpuSystemInfo.OsMode = lo.ToPtr(domain.OsModeImage)
			createDevice("gpu-device", gpuSystemInfo, map[string]string{})

			noGPUSystemInfo := reportingSystemInfo()
			noGPUSystemInfo.Gpus = &[]api.DeviceGpu{}
			noGPUSystemInfo.OsMode = lo.ToPtr(domain.OsModePackage)
			createDevice("no-gpu-device", noGPUSystemInfo, map[string]string{})

			createDevice("unreported-device", reportingSystemInfo(), map[string]string{})

			for _, name := range []string{"gpu-device", "no-gpu-device", "unreported-device"} {
				reconcile(name)
			}

			Expect(listDeviceNames(gpuPresentLabel + "=true")).To(ConsistOf("gpu-device"))
			Expect(listDeviceNames(gpuPresentLabel + "=false")).To(ConsistOf("no-gpu-device"))
			Expect(listDeviceNames(gpuPresentLabel + " in (true,false)")).To(ConsistOf("gpu-device", "no-gpu-device"))
			Expect(listDeviceNames("!" + gpuPresentLabel)).To(ConsistOf("unreported-device"))
			Expect(listDeviceNames(osModeLabel + "=image")).To(ConsistOf("gpu-device"))
		})
	})

	Context("Mapping lifecycle and failure isolation", func() {
		It("When no mappings exist it should leave the device labels untouched", func() {
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{{Index: 0}}
			createDevice("unmapped-device", systemInfo, map[string]string{"site": "east"})

			reconcile("unmapped-device")

			labels := deviceLabels("unmapped-device")
			Expect(labels).To(HaveKeyWithValue("site", "east"))
			Expect(labels).NotTo(HaveKey(gpuPresentLabel))
		})

		It("When a capability mapping is deleted it should remove the labels it owned", func() {
			provisionDefaultMappings()
			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{{Index: 0}}
			systemInfo.Kvm = &api.DeviceKvm{Enabled: lo.ToPtr(true)}
			createDevice("retired-mapping-device", systemInfo, map[string]string{"site": "east"})
			reconcile("retired-mapping-device")
			Expect(deviceLabels("retired-mapping-device")).To(HaveKeyWithValue(gpuPresentLabel, "true"))

			Expect(mappingSvc.DeleteLabelSyncMapping(ctx, orgID, "gpu-present").Code).To(Equal(int32(200)))

			reconcile("retired-mapping-device")

			labels := deviceLabels("retired-mapping-device")
			Expect(labels).NotTo(HaveKey(gpuPresentLabel))
			Expect(labels).To(HaveKeyWithValue(kvmEnabledLabel, "true"), "unrelated mappings keep deriving")
			Expect(labels).To(HaveKeyWithValue("site", "east"))
		})

		It("When one mapping fails at evaluation time it should still derive the other capability labels", func() {
			provisionDefaultMappings()
			// Compiles, but fails at runtime because the GPU list is not an integer.
			broken := domain.LabelSyncMapping{
				Metadata: domain.ObjectMeta{Name: lo.ToPtr("broken-capability")},
				Spec: domain.LabelSyncMappingSpec{
					ResourceType: domain.LabelSyncMappingDevice,
					Key:          lo.ToPtr("feature.flightctl.io/broken"),
					Expression:   "string(int(status.systemInfo.architecture))",
				},
			}
			_, status := mappingSvc.CreateLabelSyncMapping(ctx, orgID, broken)
			Expect(status.Code).To(Equal(int32(201)), status.Message)

			systemInfo := reportingSystemInfo()
			systemInfo.Gpus = &[]api.DeviceGpu{{Index: 0}}
			systemInfo.Kvm = &api.DeviceKvm{Enabled: lo.ToPtr(true)}
			systemInfo.OsMode = lo.ToPtr(domain.OsModeImage)
			createDevice("isolated-failure-device", systemInfo, map[string]string{})

			reconcile("isolated-failure-device")

			labels := deviceLabels("isolated-failure-device")
			Expect(labels).NotTo(HaveKey("feature.flightctl.io/broken"))
			Expect(labels).To(HaveKeyWithValue(gpuPresentLabel, "true"))
			Expect(labels).To(HaveKeyWithValue(kvmEnabledLabel, "true"))
			Expect(labels).To(HaveKeyWithValue(osModeLabel, string(domain.OsModeImage)))
		})
	})
})
