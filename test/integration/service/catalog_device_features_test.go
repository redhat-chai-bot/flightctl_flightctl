package service_test

import (
	"net/http"

	api "github.com/flightctl/flightctl/api/core/v1alpha1"
	apiv1beta1 "github.com/flightctl/flightctl/api/core/v1beta1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

// Device feature requirements are declared per catalog item version and compared
// client-side against the server-derived feature.flightctl.io/* device labels.
var _ = Describe("CatalogItem device feature requirements", func() {
	var (
		suite       *ServiceTestSuite
		catalogName string
	)

	BeforeEach(func() {
		suite = NewServiceTestSuite()
		suite.Setup()

		catalogName = "capability-catalog"
		_, status := suite.Catalog.CreateCatalog(suite.Ctx, suite.OrgID, api.Catalog{
			Metadata: apiv1beta1.ObjectMeta{Name: lo.ToPtr(catalogName)},
			Spec:     api.CatalogSpec{DisplayName: lo.ToPtr("Capability Catalog")},
		})
		Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)
	})

	AfterEach(func() {
		suite.Teardown()
	})

	It("When a version declares known device features it should persist and return them", func() {
		item := createValidCatalogItem("gpu-workload")
		item.Spec.Versions[0].DeviceFeatures = &api.DeviceFeatures{
			GpuPresent: lo.ToPtr(api.DeviceFeatureBooleanTrue),
			KvmEnabled: lo.ToPtr(api.DeviceFeatureBooleanFalse),
			OsMode:     lo.ToPtr(apiv1beta1.OsModeImage),
		}

		_, status := suite.Catalog.CreateCatalogItem(suite.Ctx, suite.OrgID, catalogName, item)
		Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

		fetched, status := suite.Catalog.GetCatalogItem(suite.Ctx, suite.OrgID, catalogName, "gpu-workload")
		Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
		Expect(fetched.Spec.Versions).To(HaveLen(1))
		features := fetched.Spec.Versions[0].DeviceFeatures
		Expect(features).ToNot(BeNil())
		Expect(lo.FromPtr(features.GpuPresent)).To(Equal(api.DeviceFeatureBooleanTrue))
		Expect(lo.FromPtr(features.KvmEnabled)).To(Equal(api.DeviceFeatureBooleanFalse))
		Expect(lo.FromPtr(features.OsMode)).To(Equal(apiv1beta1.OsModeImage))
	})

	It("When a version declares no device features it should be stored without requirements", func() {
		_, status := suite.Catalog.CreateCatalogItem(suite.Ctx, suite.OrgID, catalogName, createValidCatalogItem("unconstrained-workload"))
		Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

		fetched, status := suite.Catalog.GetCatalogItem(suite.Ctx, suite.OrgID, catalogName, "unconstrained-workload")
		Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
		Expect(fetched.Spec.Versions[0].DeviceFeatures).To(BeNil())
	})

	It("When successive versions declare different requirements it should keep them independent", func() {
		item := createValidCatalogItem("multi-version-workload")
		item.Spec.Versions = []api.CatalogItemVersion{
			{
				Version:        "1.0.0",
				References:     map[api.CatalogItemArtifactType]string{"container": "v1.0.0"},
				Channels:       []string{"stable"},
				DeviceFeatures: &api.DeviceFeatures{GpuPresent: lo.ToPtr(api.DeviceFeatureBooleanFalse)},
			},
			{
				Version:        "2.0.0",
				References:     map[api.CatalogItemArtifactType]string{"container": "v2.0.0"},
				Channels:       []string{"stable"},
				DeviceFeatures: &api.DeviceFeatures{GpuPresent: lo.ToPtr(api.DeviceFeatureBooleanTrue)},
			},
		}

		_, status := suite.Catalog.CreateCatalogItem(suite.Ctx, suite.OrgID, catalogName, item)
		Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

		fetched, status := suite.Catalog.GetCatalogItem(suite.Ctx, suite.OrgID, catalogName, "multi-version-workload")
		Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
		requirementsByVersion := map[string]api.DeviceFeatureBoolean{}
		for _, version := range fetched.Spec.Versions {
			Expect(version.DeviceFeatures).ToNot(BeNil())
			requirementsByVersion[version.Version] = lo.FromPtr(version.DeviceFeatures.GpuPresent)
		}
		Expect(requirementsByVersion).To(HaveKeyWithValue("1.0.0", api.DeviceFeatureBooleanFalse))
		Expect(requirementsByVersion).To(HaveKeyWithValue("2.0.0", api.DeviceFeatureBooleanTrue))
	})

	It("When a known device feature carries an out-of-enum value it should be rejected and not persisted", func() {
		item := createValidCatalogItem("invalid-requirement-workload")
		item.Spec.Versions[0].DeviceFeatures = &api.DeviceFeatures{
			GpuPresent: lo.ToPtr(api.DeviceFeatureBoolean("yes")),
		}

		_, status := suite.Catalog.CreateCatalogItem(suite.Ctx, suite.OrgID, catalogName, item)
		Expect(status.Code).To(BeEquivalentTo(http.StatusBadRequest), status.Message)

		_, status = suite.Catalog.GetCatalogItem(suite.Ctx, suite.OrgID, catalogName, "invalid-requirement-workload")
		Expect(status.Code).To(BeEquivalentTo(http.StatusNotFound), status.Message)
	})

	It("When a device feature map carries an unknown key it should be accepted for forward compatibility", func() {
		item := createValidCatalogItem("forward-compatible-workload")
		item.Spec.Versions[0].DeviceFeatures = &api.DeviceFeatures{
			GpuPresent:           lo.ToPtr(api.DeviceFeatureBooleanTrue),
			AdditionalProperties: map[string]interface{}{"cpu.architecture": "amd64"},
		}

		_, status := suite.Catalog.CreateCatalogItem(suite.Ctx, suite.OrgID, catalogName, item)
		Expect(status.Code).To(BeEquivalentTo(http.StatusCreated), status.Message)

		fetched, status := suite.Catalog.GetCatalogItem(suite.Ctx, suite.OrgID, catalogName, "forward-compatible-workload")
		Expect(status.Code).To(BeEquivalentTo(http.StatusOK), status.Message)
		features := fetched.Spec.Versions[0].DeviceFeatures
		Expect(features).ToNot(BeNil())
		Expect(lo.FromPtr(features.GpuPresent)).To(Equal(api.DeviceFeatureBooleanTrue))
		Expect(features.AdditionalProperties).To(HaveKeyWithValue("cpu.architecture", "amd64"))
	})
})
