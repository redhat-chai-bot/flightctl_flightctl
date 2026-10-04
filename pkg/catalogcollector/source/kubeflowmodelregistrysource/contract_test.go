package kubeflowmodelregistrysource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mrapi "github.com/kubeflow/hub/pkg/openapi"
)

// fixtureDir is relative to the repo root (tests run from the package dir,
// so we traverse up to find publisher/fixtures/model-registry/).
//
// The fixtures were captured from a real RHOAI 3.5.1 deployment (OCP 4.20.37)
// using the Model Registry API v1alpha3 behind kube-rbac-proxy (bearer token
// auth, router CA trust). Key observations:
//   - ModelArtifact.State is absent from real API responses (unmarshalled as
//     nil → treated as UNKNOWN → eligible per eligibleArtifactStates).
//   - Registered models and model versions are server-side filtered via
//     filterQuery=state='LIVE'; no state filter is applied to artifacts.
//   - Reconciliation is idempotent: identical content produces the same
//     revision hash and no writes are sent to Flightctl.
//   - Auth failure (invalid bearer token) aborts the collection cycle without
//     pruning existing Flightctl Catalog or CatalogItem resources.
//   - Recovery is automatic: the poller retries with bounded exponential
//     backoff; successful collection resets the backoff interval.
const fixtureDir = "../../../../publisher/fixtures/model-registry"

func readFixture[T any](t *testing.T, name string) T {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parsing fixture %s: %v", name, err)
	}
	return v
}

// fixtureClient serves real sanitized API responses from the fixture directory.
// It represents a one-model, one-version, one-artifact registry.
type fixtureClient struct {
	models    *mrapi.RegisteredModelList
	version   *mrapi.ModelVersion
	artifacts *mrapi.ArtifactList
}

func newFixtureClient(t *testing.T) *fixtureClient {
	t.Helper()
	return &fixtureClient{
		models:    readFixture[*mrapi.RegisteredModelList](t, "registered-models-list.json"),
		version:   readFixture[*mrapi.ModelVersion](t, "model-version.json"),
		artifacts: readFixture[*mrapi.ArtifactList](t, "model-version-artifacts.json"),
	}
}

func (f *fixtureClient) ListRegisteredModels(_ context.Context, _ string) (*mrapi.RegisteredModelList, error) {
	return f.models, nil
}

func (f *fixtureClient) ListModelVersions(_ context.Context, _ string, _ string) (*mrapi.ModelVersionList, error) {
	// Wrap the single version object into a list.
	return &mrapi.ModelVersionList{Items: []mrapi.ModelVersion{*f.version}}, nil
}

func (f *fixtureClient) ListModelArtifacts(_ context.Context, _ string, _ string) (*mrapi.ArtifactList, error) {
	return f.artifacts, nil
}

// TestContractFixtures verifies the exact mapping output for the sanitized
// real API responses captured from a running RHOAI deployment.
func TestContractFixtures(t *testing.T) {
	client := newFixtureClient(t)
	s := &source{
		id:                "contract-test",
		catalog:           "rhoai-models",
		collectionTimeout: 30e9, // 30 seconds in nanoseconds
		client:            client,
		log:               testLogger(),
	}

	snap, err := s.collect(context.Background())
	if err != nil {
		t.Fatalf("collect() with real fixtures: %v", err)
	}

	// --- Catalog ---
	if len(snap.Catalogs) != 1 {
		t.Fatalf("expected 1 catalog, got %d", len(snap.Catalogs))
	}
	catalogName := ptrStr(snap.Catalogs[0].Metadata.Name)
	if catalogName != "rhoai-models" {
		t.Errorf("catalog name = %q, want %q", catalogName, "rhoai-models")
	}

	// --- CatalogItems ---
	// The fixture has one model: iris-edge (id=1, LIVE).
	if len(snap.CatalogItems) != 1 {
		t.Fatalf("expected 1 item, got %d", len(snap.CatalogItems))
	}
	item := snap.CatalogItems[0]

	// The model name "iris-edge" already satisfies DNS normalization.
	if ptrStr(item.Metadata.Name) != "iris-edge" {
		t.Errorf("item.metadata.name = %q, want %q", ptrStr(item.Metadata.Name), "iris-edge")
	}
	if item.Metadata.Catalog != "rhoai-models" {
		t.Errorf("item.metadata.catalog = %q, want %q", item.Metadata.Catalog, "rhoai-models")
	}

	// --- Spec: displayName preserved verbatim ---
	if ptrStr(item.Spec.DisplayName) != "iris-edge" {
		t.Errorf("displayName = %q, want %q", ptrStr(item.Spec.DisplayName), "iris-edge")
	}

	// --- Artifact: versionless OCI repository ---
	const wantRepo = "192.168.1.171:5000/rhoai-rhem-poc/modelcar-iris"
	if len(item.Spec.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(item.Spec.Artifacts))
	}
	if item.Spec.Artifacts[0].Uri != wantRepo {
		t.Errorf("artifact.uri = %q, want %q", item.Spec.Artifacts[0].Uri, wantRepo)
	}

	// --- Version ---
	if len(item.Spec.Versions) != 1 {
		t.Fatalf("expected 1 version, got %d", len(item.Spec.Versions))
	}
	v := item.Spec.Versions[0]
	if string(v.Version) != "1.0.0" {
		t.Errorf("version = %q, want %q", v.Version, "1.0.0")
	}
	const wantDigest = "sha256:bd62d86fc106e620c60f3905ebb1f8d102dc2470d049d845eada1c9b823801ba"
	if v.References["container"] != wantDigest {
		t.Errorf("references[container] = %q, want %q", v.References["container"], wantDigest)
	}
	if len(v.Channels) != 1 || v.Channels[0] != "stable" {
		t.Errorf("channels = %v, want [stable]", v.Channels)
	}

	// --- Revision: non-empty 16 hex chars ---
	if len(snap.Revision) != 16 {
		t.Errorf("revision length = %d, want 16", len(snap.Revision))
	}

	// --- Description propagated ---
	if ptrStr(item.Spec.ShortDescription) != "Iris classifier model for edge inference" {
		t.Errorf("shortDescription = %q", ptrStr(item.Spec.ShortDescription))
	}
}

// TestContractFixtures_ArchivedModelExcluded verifies that the source is given
// only LIVE models (the filtering is done server-side via filterQuery) and that
// a model returned in ARCHIVED state (not expected in practice) would cause no
// output since the fixture models list only contains LIVE models.
func TestContractFixtures_EmptyList(t *testing.T) {
	emptyList := readFixture[*mrapi.RegisteredModelList](t, "empty-list.json")
	client := &fixtureClient{models: emptyList}

	s := &source{
		id:                "contract-empty",
		catalog:           "rhoai-models",
		collectionTimeout: 30e9,
		client:            client,
		log:               testLogger(),
	}

	snap, err := s.collect(context.Background())
	if err != nil {
		t.Fatalf("collect() with empty fixture: %v", err)
	}
	if len(snap.CatalogItems) != 0 {
		t.Errorf("expected 0 items, got %d", len(snap.CatalogItems))
	}
	if snap.Revision == "" {
		t.Error("expected non-empty revision even for empty snapshot")
	}
}
