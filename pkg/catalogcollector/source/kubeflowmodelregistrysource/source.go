package kubeflowmodelregistrysource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	mrapi "github.com/kubeflow/hub/pkg/openapi"
	"github.com/sirupsen/logrus"

	apiv1alpha1 "github.com/flightctl/flightctl/api/core/v1alpha1"
	catalogcollector "github.com/flightctl/flightctl/pkg/catalogcollector"
	"github.com/flightctl/flightctl/pkg/catalogcollector/source/pollsource"
)

// source implements catalogcollector.Source for the Kubeflow Model Registry.
type source struct {
	id                string
	catalog           string
	collectionTimeout time.Duration
	client            registryClient
	poller            *pollsource.Helper
	next              catalogcollector.Consumer
	log               *logrus.Entry
	metrics           *sourceMetrics
}

var _ catalogcollector.Source = (*source)(nil)

// Run blocks until ctx is cancelled. It drives the poll loop via the pollsource
// helper, performing a complete collection on each tick.
func (s *source) Run(ctx context.Context) error {
	s.log.Info("source starting")
	return s.poller.Run(ctx, s.collect, s.next)
}

// collect performs one complete collection cycle:
//  1. Fetch all pages of LIVE registered models.
//  2. For each model, fetch all pages of LIVE model versions.
//  3. For each version, fetch all pages of model-artifact typed artifacts and
//     select exactly one eligible artifact.
//  4. Normalize and validate the whole set, then build a CatalogSnapshot.
//
// Any error aborts the complete cycle. A partial snapshot is never emitted.
func (s *source) collect(ctx context.Context) (*catalogcollector.CatalogSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, s.collectionTimeout)
	defer cancel()

	models, err := s.fetchAllModels(ctx)
	if err != nil {
		return nil, err
	}

	collected := make([]collectedModel, 0, len(models))
	skipped := 0
	for _, model := range models {
		if model.Id == nil {
			return nil, fmt.Errorf("registered model name=%q has nil id (unexpected server response)", model.Name)
		}
		cm, err := s.fetchModel(ctx, model)
		if err != nil {
			return nil, err
		}
		if cm == nil {
			skipped++
			continue
		}
		collected = append(collected, *cm)
	}

	catalogs, items, err := toSnapshot(s.catalog, collected)
	if err != nil {
		return nil, err
	}

	revision := computeRevision(catalogs, items)
	s.log.WithFields(logrus.Fields{
		"eligible": len(items),
		"skipped":  skipped,
		"revision": revision,
	}).Debug("collection complete")

	return &catalogcollector.CatalogSnapshot{
		Revision:     revision,
		Catalogs:     catalogs,
		CatalogItems: items,
	}, nil
}

// fetchAllModels paginates GET /registered_models?filterQuery=state='LIVE'.
func (s *source) fetchAllModels(ctx context.Context) ([]mrapi.RegisteredModel, error) {
	var all []mrapi.RegisteredModel
	token := ""
	seen := map[string]bool{"": true}

	for {
		list, err := s.client.ListRegisteredModels(ctx, token)
		if err != nil {
			return nil, s.wrapHTTPError("listing registered models", err)
		}
		all = append(all, list.Items...)

		next := list.NextPageToken
		if next == "" {
			break
		}
		if seen[next] {
			return nil, fmt.Errorf("pagination loop: server returned repeated nextPageToken %q when listing registered models", next)
		}
		seen[next] = true
		token = next
	}
	return all, nil
}

// fetchModel fetches all LIVE versions and their eligible artifacts for one
// registered model. Returns nil (no error) if the model has no eligible
// versions — such models are silently excluded from the snapshot.
func (s *source) fetchModel(ctx context.Context, model mrapi.RegisteredModel) (*collectedModel, error) {
	modelID := safeID(model.Id)

	versions, err := s.fetchAllVersions(ctx, modelID)
	if err != nil {
		return nil, err
	}

	var eligible []collectedVersion
	for _, ver := range versions {
		if ver.Id == nil {
			return nil, fmt.Errorf("registered model id=%s name=%q: version name=%q has nil id", modelID, model.Name, ver.Name)
		}
		cv, err := s.fetchVersion(ctx, modelID, model.Name, ver)
		if err != nil {
			return nil, err
		}
		if cv == nil {
			continue
		}
		eligible = append(eligible, *cv)
	}

	if len(eligible) == 0 {
		// Model has no LIVE versions (either no versions at all, or all archived
		// by the server-side filter). Silently omit it from the snapshot; the
		// caller accumulates the skipped count and logs a single summary.
		return nil, nil
	}

	return &collectedModel{model: model, versions: eligible}, nil
}

// fetchAllVersions paginates GET /registered_models/{id}/versions?filterQuery=state='LIVE'.
func (s *source) fetchAllVersions(ctx context.Context, modelID string) ([]mrapi.ModelVersion, error) {
	var all []mrapi.ModelVersion
	token := ""
	seen := map[string]bool{"": true}

	for {
		list, err := s.client.ListModelVersions(ctx, modelID, token)
		if err != nil {
			return nil, s.wrapHTTPError(fmt.Sprintf("listing versions for model id=%s", modelID), err)
		}
		all = append(all, list.Items...)

		next := list.NextPageToken
		if next == "" {
			break
		}
		if seen[next] {
			return nil, fmt.Errorf("pagination loop: server returned repeated nextPageToken %q when listing versions for model id=%s", next, modelID)
		}
		seen[next] = true
		token = next
	}
	return all, nil
}

// fetchVersion fetches and validates artifacts for a single model version.
// Returns nil (no error) when the version has no eligible artifacts but still
// has an active state — the cycle fails hard when it has an ineligible artifact
// or ambiguous selection.
func (s *source) fetchVersion(ctx context.Context, modelID, modelName string, ver mrapi.ModelVersion) (*collectedVersion, error) {
	versionID := safeID(ver.Id)
	artifacts, err := s.fetchAllArtifacts(ctx, versionID)
	if err != nil {
		return nil, err
	}

	type eligible struct {
		repo   string
		digest string
	}
	var eligible_ []eligible

	for i := range artifacts {
		art := artifacts[i]
		ma, ok := extractModelArtifact(art)
		if !ok {
			continue
		}
		repo, digest, isElig, err := isEligibleArtifact(ma)
		if err != nil {
			return nil, fmt.Errorf(
				"registered model id=%s name=%q, version id=%s name=%q: %w",
				modelID, modelName, versionID, ver.Name, err)
		}
		if isElig {
			eligible_ = append(eligible_, eligible{repo, digest})
		} else {
			if ma.State != nil {
				s.log.WithFields(logrus.Fields{
					"model_id":       modelID,
					"version_id":     versionID,
					"artifact_state": string(*ma.State),
				}).Debug("artifact excluded by state filter (Open Question 9.4)")
			}
		}
	}

	switch len(eligible_) {
	case 0:
		return nil, fmt.Errorf(
			"registered model id=%s name=%q, version id=%s name=%q: "+
				"no eligible model-artifact found (requires exactly one artifact with "+
				"artifactType=model-artifact, a valid OCI uri pinned with @sha256: digest, "+
				"and an eligible state — see Open Question 9.4 for state policy)",
			modelID, modelName, versionID, ver.Name)
	case 1:
		return &collectedVersion{
			version:    ver,
			repository: eligible_[0].repo,
			digest:     eligible_[0].digest,
		}, nil
	default:
		return nil, fmt.Errorf(
			"registered model id=%s name=%q, version id=%s name=%q: "+
				"found %d eligible model-artifacts but expected exactly one; "+
				"resolve the ambiguity upstream",
			modelID, modelName, versionID, ver.Name, len(eligible_))
	}
}

// fetchAllArtifacts paginates GET /model_versions/{id}/artifacts?artifactType=model-artifact.
func (s *source) fetchAllArtifacts(ctx context.Context, versionID string) ([]mrapi.Artifact, error) {
	var all []mrapi.Artifact
	token := ""
	seen := map[string]bool{"": true}

	for {
		list, err := s.client.ListModelArtifacts(ctx, versionID, token)
		if err != nil {
			return nil, s.wrapHTTPError(fmt.Sprintf("listing artifacts for version id=%s", versionID), err)
		}
		all = append(all, list.Items...)

		next := list.NextPageToken
		if next == "" {
			break
		}
		if seen[next] {
			return nil, fmt.Errorf("pagination loop: server returned repeated nextPageToken %q when listing artifacts for version id=%s", next, versionID)
		}
		seen[next] = true
		token = next
	}
	return all, nil
}

// extractModelArtifact unwraps the polymorphic Artifact wrapper.
func extractModelArtifact(a mrapi.Artifact) (*mrapi.ModelArtifact, bool) {
	if a.ModelArtifact != nil {
		return a.ModelArtifact, true
	}
	return nil, false
}

// wrapHTTPError enriches transport / HTTP errors with actionable context while
// never leaking credentials, tokens, or authorization headers.
func (s *source) wrapHTTPError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, err)
	}

	// Check for HTTP error responses via the openapi GenericOpenAPIError type.
	var openAPIErr *mrapi.GenericOpenAPIError
	if errors.As(err, &openAPIErr) {
		// Body() may contain sensitive information; do not include it.
		return fmt.Errorf("%s: HTTP error from Model Registry (check registry connectivity and RBAC binding)", op)
	}

	return fmt.Errorf("%s: %w", op, err)
}

// computeRevision returns a deterministic content hash of the snapshot.
// Resources are sorted before hashing so the revision is stable regardless of
// upstream ordering.
func computeRevision(catalogs []apiv1alpha1.Catalog, items []apiv1alpha1.CatalogItem) string {
	// Sort catalogs by name.
	sort.Slice(catalogs, func(i, j int) bool {
		return ptrStr(catalogs[i].Metadata.Name) < ptrStr(catalogs[j].Metadata.Name)
	})
	// Sort items by catalog + name.
	sort.Slice(items, func(i, j int) bool {
		ki := items[i].Metadata.Catalog + "/" + ptrStr(items[i].Metadata.Name)
		kj := items[j].Metadata.Catalog + "/" + ptrStr(items[j].Metadata.Name)
		return ki < kj
	})

	h := sha256.New()
	enc := json.NewEncoder(h)
	for i := range catalogs {
		_ = enc.Encode(catalogs[i].Spec)
	}
	for i := range items {
		_ = enc.Encode(ptrStr(items[i].Metadata.Name))
		_ = enc.Encode(items[i].Metadata.Catalog)
		_ = enc.Encode(items[i].Spec)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
