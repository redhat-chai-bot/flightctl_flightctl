package service_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"runtime"

	"github.com/flightctl/flightctl/internal/domain"
	labelsyncmappingservice "github.com/flightctl/flightctl/internal/service/labelsyncmapping"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

const (
	gpuPresentFeatureLabel = "feature.flightctl.io/gpu.present"
	kvmEnabledFeatureLabel = "feature.flightctl.io/kvm.enabled"
)

// newCapabilityMapping returns a LabelSyncMapping that derives one feature label
// from the capability data a device reports in status.systemInfo.
func newCapabilityMapping(name, key, expression string) domain.LabelSyncMapping {
	mapping := domain.LabelSyncMapping{
		ApiVersion: "flightctl.io/v1beta1",
		Kind:       domain.LabelSyncMappingKind,
		Metadata:   domain.ObjectMeta{Name: lo.ToPtr(name)},
		Spec: domain.LabelSyncMappingSpec{
			ResourceType: domain.LabelSyncMappingDevice,
			Expression:   expression,
		},
	}
	if key != "" {
		mapping.Spec.Key = lo.ToPtr(key)
	}
	return mapping
}

func gpuPresentMapping(name string) domain.LabelSyncMapping {
	return newCapabilityMapping(name, gpuPresentFeatureLabel,
		`has(status.systemInfo.gpus) && status.systemInfo.gpus != null ? size(status.systemInfo.gpus) > 0 : dyn(null)`)
}

// readPackagedMappings decodes the mapping manifest shipped with the product so
// the API surface is exercised with the real default expressions.
func readPackagedMappings() []domain.LabelSyncMapping {
	_, sourceFile, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue(), "could not locate the test source file")
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "../../.."))
	manifestPath := filepath.Join(repoRoot, "packaging/flightctl/label-sync/mappings.yaml")

	entries, err := labelsyncmappingservice.ReadInitialLabelSyncMappingEntries(manifestPath)
	Expect(err).ToNot(HaveOccurred())
	Expect(entries).ToNot(BeEmpty(), "the packaged manifest %s must not be empty", manifestPath)

	mappings := make([]domain.LabelSyncMapping, 0, len(entries))
	for _, entry := range entries {
		var mapping domain.LabelSyncMapping
		Expect(json.Unmarshal(entry, &mapping)).To(Succeed())
		mappings = append(mappings, mapping)
	}
	return mappings
}

func readyCondition(mapping *domain.LabelSyncMapping) *domain.Condition {
	Expect(mapping).ToNot(BeNil())
	Expect(mapping.Status).ToNot(BeNil())
	return domain.FindStatusCondition(lo.FromPtr(mapping.Status.Conditions), domain.ConditionTypeLabelSyncMappingReady)
}

var _ = Describe("LabelSyncMapping Integration Tests", func() {
	var suite *ServiceTestSuite

	BeforeEach(func() {
		suite = NewServiceTestSuite()
		suite.Setup()
	})

	AfterEach(func() {
		suite.Teardown()
	})

	Context("Creating label sync mappings", func() {
		It("When a scalar capability mapping is created it should persist the derived label key", func() {
			created, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
			Expect(lo.FromPtr(created.Metadata.Name)).To(Equal("gpu-present"))
			Expect(lo.FromPtr(created.Spec.Key)).To(Equal(gpuPresentFeatureLabel))
			Expect(created.Spec.ResourceType).To(Equal(domain.LabelSyncMappingDevice))
			Expect(lo.FromPtr(created.Metadata.Generation)).To(BeEquivalentTo(1))
			Expect(lo.FromPtr(created.Metadata.ResourceVersion)).To(Equal("1"))
		})

		It("When a mapping is created it should report a pending readiness condition for its generation", func() {
			created, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

			condition := readyCondition(created)
			Expect(condition).ToNot(BeNil())
			Expect(condition.Status).To(Equal(domain.ConditionStatusFalse))
			Expect(condition.Reason).To(Equal("Pending"))
			Expect(lo.FromPtr(condition.ObservedGeneration)).To(BeEquivalentTo(1))
		})

		It("When a mapping omits a key it should accept an expression that emits a label map", func() {
			mapping := newCapabilityMapping("system-info", "",
				`status.systemInfo.transformMapEntry(k, v, k in ["architecture"], {"feature.flightctl.io/systemInfo." + k: v})`)
			created, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, mapping)
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
			Expect(created.Spec.Key).To(BeNil())
		})

		It("When every packaged default mapping is submitted it should accept all of them", func() {
			mappings := readPackagedMappings()
			Expect(mappings).To(HaveLen(5), "the packaged manifest should ship the documented capability mappings")

			names := make([]string, 0, len(mappings))
			for _, mapping := range mappings {
				created, status := labelsyncmappingservice.CreateLabelSyncMappingFromUntrusted(suite.Ctx, suite.LabelSyncMapping, suite.OrgID, mapping)
				Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
				names = append(names, lo.FromPtr(created.Metadata.Name))
			}
			Expect(names).To(ConsistOf("system-info", "custom-info", "gpu-present", "kvm-enabled", "os-mode"))
		})

		It("When a mapping name is already taken it should reject the duplicate", func() {
			_, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

			_, status = suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusConflict), status.Message)
		})

		It("When a feature label key is already claimed it should reject a second mapping for that key", func() {
			_, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

			_, status = suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present-duplicate"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusConflict), status.Message)
		})

		It("When an untrusted document carries status it should be stripped before persisting", func() {
			mapping := gpuPresentMapping("gpu-present")
			mapping.Metadata.ResourceVersion = lo.ToPtr("99")
			mapping.Metadata.Generation = lo.ToPtr(int64(42))
			mapping.Status = &domain.LabelSyncMappingStatus{Conditions: &[]domain.Condition{{
				Type:   domain.ConditionTypeLabelSyncMappingReady,
				Status: domain.ConditionStatusTrue,
				Reason: "SpoofedByClient",
			}}}

			created, status := labelsyncmappingservice.CreateLabelSyncMappingFromUntrusted(suite.Ctx, suite.LabelSyncMapping, suite.OrgID, mapping)
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
			Expect(lo.FromPtr(created.Metadata.Generation)).To(BeEquivalentTo(1))
			Expect(lo.FromPtr(created.Metadata.ResourceVersion)).To(Equal("1"))
			Expect(readyCondition(created).Reason).To(Equal("Pending"))
		})
	})

	Context("Rejecting invalid label sync mappings", func() {
		DescribeTable("invalid mapping documents",
			func(mapping domain.LabelSyncMapping, wantCode int) {
				_, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, mapping)
				Expect(status.Code).To(BeEquivalentTo(wantCode), status.Message)

				_, getStatus := suite.LabelSyncMapping.GetLabelSyncMapping(suite.Ctx, suite.OrgID, lo.FromPtr(mapping.Metadata.Name))
				Expect(getStatus.Code).To(BeEquivalentTo(http.StatusNotFound), "a rejected mapping must not be persisted")
			},
			Entry("When the CEL expression does not parse it should be rejected as unprocessable",
				newCapabilityMapping("bad-syntax", gpuPresentFeatureLabel, "status.systemInfo.gpus ?? ("), http.StatusUnprocessableEntity),
			Entry("When the CEL expression references an unknown root it should be rejected as unprocessable",
				newCapabilityMapping("unknown-root", gpuPresentFeatureLabel, "device.status.systemInfo.gpus.size() > 0"), http.StatusUnprocessableEntity),
			Entry("When a scalar mapping returns a map it should be rejected as unprocessable",
				newCapabilityMapping("scalar-returns-map", gpuPresentFeatureLabel, `{"feature.flightctl.io/gpu.present": "true"}`), http.StatusUnprocessableEntity),
			Entry("When a map mapping returns a scalar it should be rejected as unprocessable",
				newCapabilityMapping("map-returns-scalar", "", `"true"`), http.StatusUnprocessableEntity),
			Entry("When the expression is empty it should be rejected as a bad request",
				newCapabilityMapping("empty-expression", gpuPresentFeatureLabel, ""), http.StatusBadRequest),
			Entry("When the key is not a valid label key it should be rejected as a bad request",
				newCapabilityMapping("invalid-key", "feature.flightctl.io/not a key", "status.systemInfo.osMode"), http.StatusBadRequest),
			Entry("When the resource name is not valid it should be rejected as a bad request",
				newCapabilityMapping("Not_A_Valid_Name", gpuPresentFeatureLabel, "status.systemInfo.osMode"), http.StatusBadRequest),
		)

		It("When the resource type is not Device it should be rejected as a bad request", func() {
			mapping := gpuPresentMapping("wrong-resource-type")
			mapping.Spec.ResourceType = domain.LabelSyncMappingResourceType("Fleet")

			_, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, mapping)
			Expect(status.Code).To(BeEquivalentTo(http.StatusBadRequest), status.Message)
		})
	})

	Context("Reading label sync mappings", func() {
		It("When a mapping exists it should be returned by name", func() {
			_, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

			fetched, status := suite.LabelSyncMapping.GetLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present")
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(lo.FromPtr(fetched.Spec.Key)).To(Equal(gpuPresentFeatureLabel))
		})

		It("When a mapping does not exist it should report not found", func() {
			_, status := suite.LabelSyncMapping.GetLabelSyncMapping(suite.Ctx, suite.OrgID, "missing-mapping")
			Expect(status.Code).To(BeEquivalentTo(http.StatusNotFound), status.Message)
		})

		It("When no mappings exist it should return an empty list", func() {
			list, status := suite.LabelSyncMapping.ListLabelSyncMappings(suite.Ctx, suite.OrgID, domain.ListLabelSyncMappingsParams{})
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(list.Items).To(BeEmpty())
		})

		It("When several mappings exist it should list all of them and support paging", func() {
			for _, mapping := range []domain.LabelSyncMapping{
				gpuPresentMapping("gpu-present"),
				newCapabilityMapping("kvm-enabled", kvmEnabledFeatureLabel,
					`has(status.systemInfo.kvm) && status.systemInfo.kvm != null && has(status.systemInfo.kvm.enabled) && status.systemInfo.kvm.enabled != null ? status.systemInfo.kvm.enabled : dyn(null)`),
				newCapabilityMapping("os-mode", "feature.flightctl.io/os.mode", "status.systemInfo.osMode"),
			} {
				_, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, mapping)
				Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
			}

			list, status := suite.LabelSyncMapping.ListLabelSyncMappings(suite.Ctx, suite.OrgID, domain.ListLabelSyncMappingsParams{})
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(list.Items).To(HaveLen(3))

			page, status := suite.LabelSyncMapping.ListLabelSyncMappings(suite.Ctx, suite.OrgID, domain.ListLabelSyncMappingsParams{
				Limit: lo.ToPtr(int32(2)),
			})
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(page.Items).To(HaveLen(2))
			Expect(page.Metadata.Continue).ToNot(BeNil(), "a truncated list must offer a continue token")

			rest, status := suite.LabelSyncMapping.ListLabelSyncMappings(suite.Ctx, suite.OrgID, domain.ListLabelSyncMappingsParams{
				Limit:    lo.ToPtr(int32(2)),
				Continue: page.Metadata.Continue,
			})
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(rest.Items).To(HaveLen(1))
		})
	})

	Context("Replacing label sync mappings", func() {
		var existing *domain.LabelSyncMapping

		BeforeEach(func() {
			var status domain.Status
			existing, status = suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
		})

		It("When the expression changes it should advance the generation and re-arm the pending condition", func() {
			updated := *existing
			updated.Spec.Expression = `has(status.systemInfo.gpus) && status.systemInfo.gpus != null ? size(status.systemInfo.gpus) > 1 : dyn(null)`

			replaced, status := suite.LabelSyncMapping.ReplaceLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present", updated)
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(lo.FromPtr(replaced.Metadata.Generation)).To(BeEquivalentTo(2))
			Expect(lo.FromPtr(readyCondition(replaced).ObservedGeneration)).To(BeEquivalentTo(2))
		})

		It("When the spec is unchanged it should keep the generation stable", func() {
			replaced, status := suite.LabelSyncMapping.ReplaceLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present", *existing)
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(lo.FromPtr(replaced.Metadata.Generation)).To(BeEquivalentTo(1))
		})

		It("When the body names a different mapping it should be rejected as a bad request", func() {
			updated := *existing
			updated.Metadata.Name = lo.ToPtr("another-mapping")

			_, status := suite.LabelSyncMapping.ReplaceLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present", updated)
			Expect(status.Code).To(BeEquivalentTo(http.StatusBadRequest), status.Message)
		})

		It("When the replacement expression is invalid it should be rejected and leave the mapping unchanged", func() {
			updated := *existing
			updated.Spec.Expression = "status.systemInfo.gpus ?? ("

			_, status := suite.LabelSyncMapping.ReplaceLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present", updated)
			Expect(status.Code).To(BeEquivalentTo(http.StatusUnprocessableEntity), status.Message)

			unchanged, status := suite.LabelSyncMapping.GetLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present")
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(unchanged.Spec.Expression).To(Equal(existing.Spec.Expression))
			Expect(lo.FromPtr(unchanged.Metadata.Generation)).To(BeEquivalentTo(1))
		})

		It("When the replacement claims another mapping's key it should report a conflict", func() {
			other := newCapabilityMapping("kvm-enabled", kvmEnabledFeatureLabel,
				`has(status.systemInfo.kvm) && status.systemInfo.kvm != null && has(status.systemInfo.kvm.enabled) && status.systemInfo.kvm.enabled != null ? status.systemInfo.kvm.enabled : dyn(null)`)
			created, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, other)
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

			updated := *created
			updated.Spec.Key = lo.ToPtr(gpuPresentFeatureLabel)
			_, status = suite.LabelSyncMapping.ReplaceLabelSyncMapping(suite.Ctx, suite.OrgID, "kvm-enabled", updated)
			Expect(status.Code).To(BeEquivalentTo(http.StatusConflict), status.Message)
		})

		It("When the mapping does not exist it should report not found", func() {
			_, status := suite.LabelSyncMapping.ReplaceLabelSyncMapping(suite.Ctx, suite.OrgID, "missing-mapping", gpuPresentMapping("missing-mapping"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusNotFound), status.Message)
		})
	})

	Context("Patching label sync mappings", func() {
		BeforeEach(func() {
			_, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
		})

		It("When the expression is patched it should persist the new expression", func() {
			expression := `has(status.systemInfo.gpus) && status.systemInfo.gpus != null ? size(status.systemInfo.gpus) > 2 : dyn(null)`
			patch := domain.PatchRequest{{Op: "replace", Path: "/spec/expression", Value: expression}}

			patched, status := suite.LabelSyncMapping.PatchLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present", patch)
			Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
			Expect(patched.Spec.Expression).To(Equal(expression))
			Expect(lo.FromPtr(patched.Metadata.Generation)).To(BeEquivalentTo(2))
		})

		It("When the patch produces an invalid expression it should be rejected as unprocessable", func() {
			patch := domain.PatchRequest{{Op: "replace", Path: "/spec/expression", Value: "status.systemInfo.gpus ?? ("}}

			_, status := suite.LabelSyncMapping.PatchLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present", patch)
			Expect(status.Code).To(BeEquivalentTo(http.StatusUnprocessableEntity), status.Message)
		})

		It("When the patch renames the mapping it should be rejected as a bad request", func() {
			patch := domain.PatchRequest{{Op: "replace", Path: "/metadata/name", Value: "renamed-mapping"}}

			_, status := suite.LabelSyncMapping.PatchLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present", patch)
			Expect(status.Code).To(BeEquivalentTo(http.StatusBadRequest), status.Message)
		})

		It("When the mapping does not exist it should report not found", func() {
			patch := domain.PatchRequest{{Op: "replace", Path: "/spec/expression", Value: "status.systemInfo.osMode"}}

			_, status := suite.LabelSyncMapping.PatchLabelSyncMapping(suite.Ctx, suite.OrgID, "missing-mapping", patch)
			Expect(status.Code).To(BeEquivalentTo(http.StatusNotFound), status.Message)
		})
	})

	Context("Deleting label sync mappings", func() {
		It("When a mapping without owned labels is deleted it should disappear and release its key", func() {
			_, status := suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

			Expect(suite.LabelSyncMapping.DeleteLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present").Code).
				To(BeEquivalentTo(http.StatusOK))

			_, getStatus := suite.LabelSyncMapping.GetLabelSyncMapping(suite.Ctx, suite.OrgID, "gpu-present")
			Expect(getStatus.Code).To(BeEquivalentTo(http.StatusNotFound))

			_, status = suite.LabelSyncMapping.CreateLabelSyncMapping(suite.Ctx, suite.OrgID, gpuPresentMapping("gpu-present-replacement"))
			Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
		})

		It("When the mapping does not exist it should succeed idempotently", func() {
			Expect(suite.LabelSyncMapping.DeleteLabelSyncMapping(suite.Ctx, suite.OrgID, "missing-mapping").Code).
				To(BeEquivalentTo(http.StatusOK))
		})
	})
})
