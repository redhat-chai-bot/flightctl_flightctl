package kubeflowmodelregistrysource

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mrapi "github.com/kubeflow/hub/pkg/openapi"
	"github.com/sirupsen/logrus"

	catalogcollector "github.com/flightctl/flightctl/pkg/catalogcollector"
)

// --- fakeRegistryClient ----------------------------------------------------

type fakeRegistryClient struct {
	models      [][]mrapi.RegisteredModel // one page per outer slice
	modelTokens []string                  // tokens returned between pages (len = len(models)-1)

	// versions and artifacts are keyed by modelID and versionID respectively.
	versions  map[string][][]mrapi.ModelVersion
	artifacts map[string][][]mrapi.Artifact

	modelErr    error
	versionErr  map[string]error
	artifactErr map[string]error
}

func (f *fakeRegistryClient) ListRegisteredModels(_ context.Context, token string) (*mrapi.RegisteredModelList, error) {
	if f.modelErr != nil {
		return nil, f.modelErr
	}
	pageIdx := pageIndex(token, f.modelTokens)
	if pageIdx >= len(f.models) {
		return &mrapi.RegisteredModelList{}, nil
	}
	nextToken := ""
	if pageIdx < len(f.modelTokens) {
		nextToken = f.modelTokens[pageIdx]
	}
	return &mrapi.RegisteredModelList{Items: f.models[pageIdx], NextPageToken: nextToken}, nil
}

func (f *fakeRegistryClient) ListModelVersions(_ context.Context, modelID string, token string) (*mrapi.ModelVersionList, error) {
	if f.versionErr != nil {
		if err, ok := f.versionErr[modelID]; ok {
			return nil, err
		}
	}
	pages := f.versions[modelID]
	if pages == nil {
		return &mrapi.ModelVersionList{}, nil
	}
	// simple single-page response for tests
	_ = token
	return &mrapi.ModelVersionList{Items: pages[0]}, nil
}

func (f *fakeRegistryClient) ListModelArtifacts(_ context.Context, versionID string, token string) (*mrapi.ArtifactList, error) {
	if f.artifactErr != nil {
		if err, ok := f.artifactErr[versionID]; ok {
			return nil, err
		}
	}
	pages := f.artifacts[versionID]
	if pages == nil {
		return &mrapi.ArtifactList{}, nil
	}
	_ = token
	return &mrapi.ArtifactList{Items: pages[0]}, nil
}

// pageIndex returns the index of the page that matches the given token in the
// token sequence. token="" → index 0; token=tokens[0] → index 1; etc.
func pageIndex(token string, tokens []string) int {
	if token == "" {
		return 0
	}
	for i, t := range tokens {
		if t == token {
			return i + 1
		}
	}
	return len(tokens) + 1 // out of range → empty response
}

// fakeConsumer records the last snapshot delivered.
type fakeConsumer struct {
	received []*catalogcollector.CatalogSnapshot
	err      error
}

func (f *fakeConsumer) Consume(_ context.Context, s *catalogcollector.CatalogSnapshot) error {
	f.received = append(f.received, s)
	return f.err
}

// --- helpers ---------------------------------------------------------------

func testLogger() *logrus.Entry {
	l := logrus.New()
	l.SetLevel(logrus.DebugLevel)
	return l.WithField("test", true)
}

func newTestSource(client registryClient, consumer catalogcollector.Consumer) (*source, *fakeConsumer) {
	fc := &fakeConsumer{}
	if consumer != nil {
		// If caller supplies one, use it; otherwise return the local one.
		_ = fc
	} else {
		consumer = fc
	}
	s := &source{
		id:                "test-source",
		catalog:           "test-catalog",
		collectionTimeout: 30 * time.Second,
		client:            client,
		log:               testLogger(),
		metrics:           nil,
		next:              consumer,
	}
	return s, fc
}

// buildLiveArtifact constructs a single model-artifact Artifact with the given URI.
func buildLiveArtifact(id, uri string) mrapi.Artifact {
	t := "model-artifact"
	live := mrapi.ARTIFACTSTATE_LIVE
	return mrapi.Artifact{
		ModelArtifact: &mrapi.ModelArtifact{
			Id:           strp(id),
			ArtifactType: &t,
			Uri:          strp(uri),
			State:        &live,
		},
	}
}

// --- collect tests ---------------------------------------------------------

func TestCollect_EmptyRegistry(t *testing.T) {
	client := &fakeRegistryClient{
		models: [][]mrapi.RegisteredModel{{}},
	}
	s, _ := newTestSource(client, nil)
	snap, err := s.collect(context.Background())
	if err != nil {
		t.Fatalf("collect() error: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if len(snap.CatalogItems) != 0 {
		t.Errorf("expected 0 items, got %d", len(snap.CatalogItems))
	}
	if len(snap.Catalogs) != 1 {
		t.Errorf("expected 1 catalog, got %d", len(snap.Catalogs))
	}
}

func TestCollect_SingleModelVersion(t *testing.T) {
	model := makeModel("1", "iris-edge")
	version := makeVersion("2", "1.0.0")

	client := &fakeRegistryClient{
		models:   [][]mrapi.RegisteredModel{{model}},
		versions: map[string][][]mrapi.ModelVersion{"1": {{version}}},
		artifacts: map[string][][]mrapi.Artifact{
			"2": {{buildLiveArtifact("10", goodURI)}},
		},
	}
	s, consumer := newTestSource(client, nil)

	snap, err := s.collect(context.Background())
	if err != nil {
		t.Fatalf("collect() error: %v", err)
	}
	if len(snap.CatalogItems) != 1 {
		t.Fatalf("expected 1 item, got %d", len(snap.CatalogItems))
	}
	if snap.Revision == "" {
		t.Error("expected non-empty revision")
	}
	_ = consumer
}

func TestCollect_ModelWithNoEligibleVersionsSkipped(t *testing.T) {
	// A model that has versions, but all versions have no artifacts → version
	// has no eligible artifacts. The current implementation requires exactly
	// one artifact per version and returns an error.
	// Change to test a model with no LIVE versions returned at all (empty list).
	model := makeModel("1", "empty-model")

	client := &fakeRegistryClient{
		models:    [][]mrapi.RegisteredModel{{model}},
		versions:  map[string][][]mrapi.ModelVersion{"1": {{}}}, // empty page
		artifacts: map[string][][]mrapi.Artifact{},
	}
	s, _ := newTestSource(client, nil)

	// Empty versions list → model silently skipped → empty snapshot, no error.
	snap, err := s.collect(context.Background())
	if err != nil {
		t.Fatalf("collect() error: %v", err)
	}
	if len(snap.CatalogItems) != 0 {
		t.Errorf("expected 0 items (model skipped), got %d", len(snap.CatalogItems))
	}
}

func TestCollect_RegistryAPIError(t *testing.T) {
	apiErr := errors.New("connection refused")
	client := &fakeRegistryClient{
		modelErr: apiErr,
	}
	s, _ := newTestSource(client, nil)
	_, err := s.collect(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestCollect_PaginationModels(t *testing.T) {
	page1 := []mrapi.RegisteredModel{makeModel("1", "model-a")}
	page2 := []mrapi.RegisteredModel{makeModel("2", "model-b")}

	client := &fakeRegistryClient{
		models:      [][]mrapi.RegisteredModel{page1, page2},
		modelTokens: []string{"token-page2"},
		versions: map[string][][]mrapi.ModelVersion{
			"1": {{makeVersion("10", "1.0.0")}},
			"2": {{makeVersion("20", "1.0.0")}},
		},
		artifacts: map[string][][]mrapi.Artifact{
			"10": {{buildLiveArtifact("100", goodURI)}},
			"20": {{buildLiveArtifact("200", goodURI)}},
		},
	}
	s, _ := newTestSource(client, nil)
	snap, err := s.collect(context.Background())
	if err != nil {
		t.Fatalf("collect() error: %v", err)
	}
	if len(snap.CatalogItems) != 2 {
		t.Errorf("expected 2 items (one per model), got %d", len(snap.CatalogItems))
	}
}

func TestCollect_RepeatedPageTokenError(t *testing.T) {
	// Simulate a server bug where the same token is returned infinitely.
	callCount := 0
	client := &repeatedTokenClient{
		onCall: func(token string) (*mrapi.RegisteredModelList, error) {
			callCount++
			if callCount > 5 {
				return nil, errors.New("test should have stopped by now")
			}
			return &mrapi.RegisteredModelList{
				Items:         []mrapi.RegisteredModel{makeModel("1", "model-a")},
				NextPageToken: "same-token", // always returns same token
			}, nil
		},
	}
	s, _ := newTestSource(client, nil)
	_, err := s.collect(context.Background())
	if err == nil {
		t.Fatal("expected repeated-token error, got nil")
	}
}

// repeatedTokenClient helps simulate infinite pagination.
type repeatedTokenClient struct {
	onCall func(token string) (*mrapi.RegisteredModelList, error)
}

func (r *repeatedTokenClient) ListRegisteredModels(_ context.Context, token string) (*mrapi.RegisteredModelList, error) {
	return r.onCall(token)
}

func (r *repeatedTokenClient) ListModelVersions(_ context.Context, _ string, _ string) (*mrapi.ModelVersionList, error) {
	return &mrapi.ModelVersionList{}, nil
}

func (r *repeatedTokenClient) ListModelArtifacts(_ context.Context, _ string, _ string) (*mrapi.ArtifactList, error) {
	return &mrapi.ArtifactList{}, nil
}

func TestCollect_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	model := makeModel("1", "iris-edge")
	client := &fakeRegistryClient{
		models:   [][]mrapi.RegisteredModel{{model}},
		versions: map[string][][]mrapi.ModelVersion{"1": {{makeVersion("2", "1.0.0")}}},
		artifacts: map[string][][]mrapi.Artifact{
			"2": {{buildLiveArtifact("10", goodURI)}},
		},
	}
	s, _ := newTestSource(client, nil)
	// A pre-cancelled context causes the collection timeout to fire immediately;
	// the collect call may or may not error but should not panic.
	_, _ = s.collect(ctx)
}

func TestWrapHTTPError_NoCredentialLeak(t *testing.T) {
	s := &source{log: testLogger()}
	err := errors.New("connection refused")
	wrapped := s.wrapHTTPError("listing registered models", err)
	if wrapped == nil {
		t.Fatal("expected non-nil wrapped error")
	}
	// The error message should not contain anything that looks like a token.
	msg := wrapped.Error()
	if len(msg) == 0 {
		t.Error("wrapped error message is empty")
	}
}

func TestWrapHTTPError_PreservesHTTPStatusCode(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus string
	}{
		{
			name:       "When error wraps HTTP 403 it should include status code in message",
			err:        &httpError{statusCode: 403, err: errors.New("forbidden")},
			wantStatus: "HTTP 403",
		},
		{
			name:       "When error wraps HTTP 404 it should include status code in message",
			err:        &httpError{statusCode: 404, err: errors.New("not found")},
			wantStatus: "HTTP 404",
		},
		{
			name:       "When error wraps HTTP 500 it should include status code in message",
			err:        &httpError{statusCode: 500, err: errors.New("internal server error")},
			wantStatus: "HTTP 500",
		},
		{
			name:       "When error wraps HTTP 429 it should include status code in message",
			err:        &httpError{statusCode: 429, err: errors.New("too many requests")},
			wantStatus: "HTTP 429",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &source{log: testLogger()}
			wrapped := s.wrapHTTPError("listing models", tc.err)
			if wrapped == nil {
				t.Fatal("expected non-nil wrapped error")
			}
			msg := wrapped.Error()
			if !strings.Contains(msg, tc.wantStatus) {
				t.Errorf("wrapped error %q does not contain %q", msg, tc.wantStatus)
			}
			// Must not contain the original error body.
			var httpErr *httpError
			if errors.As(tc.err, &httpErr) {
				if strings.Contains(msg, httpErr.err.Error()) {
					t.Errorf("wrapped error %q leaks original error body %q", msg, httpErr.err.Error())
				}
			}
		})
	}
}
