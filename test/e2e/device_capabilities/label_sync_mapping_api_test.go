package device_capabilities

import (
	"fmt"
	"net/http"

	api "github.com/flightctl/flightctl/api/core/v1beta1"
	"github.com/flightctl/flightctl/test/harness/e2e"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

const (
	gpuPresentFeatureLabel = "feature.flightctl.io/gpu.present"
	kvmEnabledFeatureLabel = "feature.flightctl.io/kvm.enabled"
	osModeFeatureLabel     = "feature.flightctl.io/os.mode"
)

// defaultCapabilityMappingKeys lists the scalar feature labels the deployment is
// expected to derive out of the box.
var defaultCapabilityMappingKeys = map[string]string{
	"gpu-present": gpuPresentFeatureLabel,
	"kvm-enabled": kvmEnabledFeatureLabel,
	"os-mode":     osModeFeatureLabel,
}

// defaultMapMappingNames lists the default mappings that emit a label map rather
// than a single key.
var defaultMapMappingNames = []string{"system-info", "custom-info"}

// listAllLabelSyncMappings pages through every mapping visible to the test user.
func listAllLabelSyncMappings(harness *e2e.Harness) []api.LabelSyncMapping {
	GinkgoHelper()

	var all []api.LabelSyncMapping
	params := &api.ListLabelSyncMappingsParams{Limit: lo.ToPtr(int32(100))}
	for {
		response, err := harness.Client.ListLabelSyncMappingsWithResponse(harness.Context, params)
		Expect(err).ToNot(HaveOccurred())
		Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusOK), string(response.Body))
		Expect(response.JSON200).ToNot(BeNil())

		all = append(all, response.JSON200.Items...)
		if response.JSON200.Metadata.Continue == nil {
			return all
		}
		params.Continue = response.JSON200.Metadata.Continue
	}
}

func labelSyncMappingNames(mappings []api.LabelSyncMapping) []string {
	names := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		names = append(names, lo.FromPtr(mapping.Metadata.Name))
	}
	return names
}

// probeExpression builds a valid scalar expression that never emits a value,
// because the probed systemInfo field does not exist. Mappings created by these
// specs must not take ownership of labels on devices owned by other suites.
func probeExpression(probe string) string {
	return fmt.Sprintf("has(status.systemInfo.%s) ? status.systemInfo.%s : dyn(null)", probe, probe)
}

// newTestMapping builds a uniquely named mapping so parallel workers never
// compete for the same label key.
func newTestMapping(harness *e2e.Harness, suffix, expression string) api.LabelSyncMapping {
	testID := harness.GetTestIDFromContext()
	name := fmt.Sprintf("e2e-%s-%s", suffix, testID)
	return api.LabelSyncMapping{
		ApiVersion: "flightctl.io/v1beta1",
		Kind:       api.LabelSyncMappingKindLabelSyncMapping,
		Metadata:   api.ObjectMeta{Name: lo.ToPtr(name)},
		Spec: api.LabelSyncMappingSpec{
			ResourceType: api.LabelSyncMappingSpecResourceTypeDevice,
			Key:          lo.ToPtr(fmt.Sprintf("feature.flightctl.io/e2e-%s-%s", suffix, testID)),
			Expression:   expression,
		},
	}
}

// createMapping creates a mapping and schedules its removal at the end of the spec.
func createMapping(harness *e2e.Harness, mapping api.LabelSyncMapping) *api.LabelSyncMapping {
	GinkgoHelper()

	name := lo.FromPtr(mapping.Metadata.Name)
	response, err := harness.Client.CreateLabelSyncMappingWithResponse(harness.Context, mapping)
	Expect(err).ToNot(HaveOccurred())
	Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusCreated), string(response.Body))
	Expect(response.JSON201).ToNot(BeNil())

	DeferCleanup(func() {
		deleteResponse, deleteErr := harness.Client.DeleteLabelSyncMappingWithResponse(harness.Context, name)
		Expect(deleteErr).ToNot(HaveOccurred())
		Expect(deleteResponse.HTTPResponse.StatusCode).To(BeElementOf(http.StatusOK, http.StatusNotFound), string(deleteResponse.Body))
	})
	return response.JSON201
}

var _ = Describe("Device capability label sync mappings", Label("integration"), func() {
	var harness *e2e.Harness

	BeforeEach(func() {
		harness = e2e.GetWorkerHarness()
	})

	Context("Default capability mappings shipped with the deployment", func() {
		It("When the deployment is installed it should expose the default feature label mappings over the API", func() {
			mappings := listAllLabelSyncMappings(harness)
			Expect(labelSyncMappingNames(mappings)).To(ContainElements(defaultMapMappingNames))

			byName := make(map[string]api.LabelSyncMapping, len(mappings))
			for _, mapping := range mappings {
				byName[lo.FromPtr(mapping.Metadata.Name)] = mapping
			}

			for name, expectedKey := range defaultCapabilityMappingKeys {
				mapping, found := byName[name]
				Expect(found).To(BeTrue(), "default mapping %q should be installed", name)
				Expect(mapping.Spec.ResourceType).To(Equal(api.LabelSyncMappingSpecResourceTypeDevice))
				Expect(lo.FromPtr(mapping.Spec.Key)).To(Equal(expectedKey))
				Expect(mapping.Spec.Expression).ToNot(BeEmpty())
			}

			for _, name := range defaultMapMappingNames {
				mapping, found := byName[name]
				Expect(found).To(BeTrue(), "default mapping %q should be installed", name)
				Expect(mapping.Spec.Key).To(BeNil(), "mapping %q should emit a label map", name)
			}
		})

		It("When a default capability mapping is fetched by name it should be readable", func() {
			response, err := harness.Client.GetLabelSyncMappingWithResponse(harness.Context, "gpu-present")
			Expect(err).ToNot(HaveOccurred())
			Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusOK), string(response.Body))
			Expect(lo.FromPtr(response.JSON200.Spec.Key)).To(Equal(gpuPresentFeatureLabel))
		})

		It("When a feature label key is already owned by a default mapping it should reject a competing mapping", func() {
			mapping := newTestMapping(harness, "gpu-conflict", probeExpression("e2eGpuConflictProbe"))
			mapping.Spec.Key = lo.ToPtr(gpuPresentFeatureLabel)

			response, err := harness.Client.CreateLabelSyncMappingWithResponse(harness.Context, mapping)
			Expect(err).ToNot(HaveOccurred())
			Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusConflict), string(response.Body))
		})
	})

	Context("Label sync mapping CRUD over the API", func() {
		It("When a mapping is created it should be retrievable, listable, replaceable, patchable, and deletable", func() {
			mapping := newTestMapping(harness, "crud", probeExpression("e2eCrudProbeOne"))
			name := lo.FromPtr(mapping.Metadata.Name)

			By("creating the mapping")
			created := createMapping(harness, mapping)
			Expect(lo.FromPtr(created.Metadata.Generation)).To(BeEquivalentTo(1))

			By("reading the mapping back by name")
			getResponse, err := harness.Client.GetLabelSyncMappingWithResponse(harness.Context, name)
			Expect(err).ToNot(HaveOccurred())
			Expect(getResponse.HTTPResponse.StatusCode).To(Equal(http.StatusOK), string(getResponse.Body))
			Expect(getResponse.JSON200.Spec.Expression).To(Equal(probeExpression("e2eCrudProbeOne")))

			By("finding the mapping in the list response")
			Expect(labelSyncMappingNames(listAllLabelSyncMappings(harness))).To(ContainElement(name))

			By("replacing the mapping expression")
			replacement := *getResponse.JSON200
			replacement.Spec.Expression = probeExpression("e2eCrudProbeTwo")
			replaceResponse, err := harness.Client.ReplaceLabelSyncMappingWithResponse(harness.Context, name, replacement)
			Expect(err).ToNot(HaveOccurred())
			Expect(replaceResponse.HTTPResponse.StatusCode).To(Equal(http.StatusOK), string(replaceResponse.Body))
			Expect(replaceResponse.JSON200.Spec.Expression).To(Equal(probeExpression("e2eCrudProbeTwo")))
			Expect(lo.FromPtr(replaceResponse.JSON200.Metadata.Generation)).To(BeEquivalentTo(2))

			By("patching the mapping expression")
			patch := api.PatchRequest{{Op: "replace", Path: "/spec/expression", Value: probeExpression("e2eCrudProbeThree")}}
			patchResponse, err := harness.Client.PatchLabelSyncMappingWithApplicationJSONPatchPlusJSONBodyWithResponse(harness.Context, name, patch)
			Expect(err).ToNot(HaveOccurred())
			Expect(patchResponse.HTTPResponse.StatusCode).To(Equal(http.StatusOK), string(patchResponse.Body))
			Expect(patchResponse.JSON200.Spec.Expression).To(Equal(probeExpression("e2eCrudProbeThree")))

			By("deleting the mapping")
			deleteResponse, err := harness.Client.DeleteLabelSyncMappingWithResponse(harness.Context, name)
			Expect(err).ToNot(HaveOccurred())
			Expect(deleteResponse.HTTPResponse.StatusCode).To(Equal(http.StatusOK), string(deleteResponse.Body))

			By("confirming the mapping is gone")
			Eventually(func() int {
				response, getErr := harness.Client.GetLabelSyncMappingWithResponse(harness.Context, name)
				Expect(getErr).ToNot(HaveOccurred())
				return response.HTTPResponse.StatusCode
			}, TIMEOUT, POLLING).Should(Equal(http.StatusNotFound))
		})

		It("When a mapping name is already taken it should reject the duplicate", func() {
			mapping := newTestMapping(harness, "duplicate", probeExpression("e2eDuplicateProbe"))
			createMapping(harness, mapping)

			response, err := harness.Client.CreateLabelSyncMappingWithResponse(harness.Context, mapping)
			Expect(err).ToNot(HaveOccurred())
			Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusConflict), string(response.Body))
		})

		It("When an unknown mapping is requested it should report not found", func() {
			response, err := harness.Client.GetLabelSyncMappingWithResponse(harness.Context, "e2e-mapping-does-not-exist")
			Expect(err).ToNot(HaveOccurred())
			Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusNotFound), string(response.Body))
		})
	})

	Context("Rejecting invalid capability mappings", func() {
		It("When the CEL expression does not compile it should be rejected and not persisted", func() {
			mapping := newTestMapping(harness, "bad-syntax", "status.systemInfo.gpus ?? (")
			name := lo.FromPtr(mapping.Metadata.Name)

			response, err := harness.Client.CreateLabelSyncMappingWithResponse(harness.Context, mapping)
			Expect(err).ToNot(HaveOccurred())
			Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusUnprocessableEntity), string(response.Body))

			getResponse, err := harness.Client.GetLabelSyncMappingWithResponse(harness.Context, name)
			Expect(err).ToNot(HaveOccurred())
			Expect(getResponse.HTTPResponse.StatusCode).To(Equal(http.StatusNotFound), string(getResponse.Body))
		})

		It("When a keyed mapping returns a label map it should be rejected", func() {
			mapping := newTestMapping(harness, "kind-mismatch", `{"feature.flightctl.io/gpu.present": "true"}`)

			response, err := harness.Client.CreateLabelSyncMappingWithResponse(harness.Context, mapping)
			Expect(err).ToNot(HaveOccurred())
			Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusUnprocessableEntity), string(response.Body))
		})

		It("When the expression is empty it should be rejected as a bad request", func() {
			mapping := newTestMapping(harness, "empty-expression", "")

			response, err := harness.Client.CreateLabelSyncMappingWithResponse(harness.Context, mapping)
			Expect(err).ToNot(HaveOccurred())
			Expect(response.HTTPResponse.StatusCode).To(Equal(http.StatusBadRequest), string(response.Body))
		})

		It("When an existing mapping is replaced with an invalid expression it should keep the stored expression", func() {
			mapping := newTestMapping(harness, "invalid-replace", probeExpression("e2eInvalidReplaceProbe"))
			name := lo.FromPtr(mapping.Metadata.Name)
			created := createMapping(harness, mapping)

			invalid := *created
			invalid.Spec.Expression = "status.systemInfo.gpus ?? ("
			replaceResponse, err := harness.Client.ReplaceLabelSyncMappingWithResponse(harness.Context, name, invalid)
			Expect(err).ToNot(HaveOccurred())
			Expect(replaceResponse.HTTPResponse.StatusCode).To(Equal(http.StatusUnprocessableEntity), string(replaceResponse.Body))

			getResponse, err := harness.Client.GetLabelSyncMappingWithResponse(harness.Context, name)
			Expect(err).ToNot(HaveOccurred())
			Expect(getResponse.HTTPResponse.StatusCode).To(Equal(http.StatusOK), string(getResponse.Body))
			Expect(getResponse.JSON200.Spec.Expression).To(Equal(probeExpression("e2eInvalidReplaceProbe")))
		})
	})
})
