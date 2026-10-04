package kubeflowmodelregistrysource

import (
	"fmt"
	"regexp"
	"strings"

	mrapi "github.com/kubeflow/hub/pkg/openapi"

	apiv1alpha1 "github.com/flightctl/flightctl/api/core/v1alpha1"
	apiv1beta1 "github.com/flightctl/flightctl/api/core/v1beta1"
)

// eligibleArtifactStates is the set of ArtifactState values that make a
// model-artifact eligible for collection.
//
// OPEN QUESTION 9.4: ArtifactState defaults to UNKNOWN for ModelArtifact.
// Real RHOAI-created ModelCar artifacts may carry LIVE or remain UNKNOWN.
// This set is an explicit interim policy; validate against a real RHOAI
// deployment and update before GA. Nil state (absent field) is treated as
// UNKNOWN.
var eligibleArtifactStates = map[mrapi.ArtifactState]bool{
	mrapi.ARTIFACTSTATE_LIVE:    true,
	mrapi.ARTIFACTSTATE_UNKNOWN: true,
}

// ociDigestRe matches a valid OCI digest: sha256: followed by exactly 64
// lowercase hex characters (OCI image-spec v1.1.0).
var ociDigestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// collectedModel is the normalized intermediate representation of a registered
// model and all its eligible versions before being converted to a CatalogItem.
type collectedModel struct {
	model    mrapi.RegisteredModel
	versions []collectedVersion
}

// collectedVersion is the normalized intermediate representation of a model
// version and its single eligible artifact.
type collectedVersion struct {
	version    mrapi.ModelVersion
	repository string // version-less OCI repository (scheme stripped)
	digest     string // sha256:… reference
}

// toSnapshot converts a slice of collected models into the Catalog and
// CatalogItems that form the snapshot.
//
// It enforces all mapping invariants (shared repository, collision detection)
// and fails the whole snapshot on any violation.
func toSnapshot(catalogName string, models []collectedModel) ([]apiv1alpha1.Catalog, []apiv1alpha1.CatalogItem, error) {
	catalog := buildCatalog(catalogName)

	seenNames := make(map[string]string) // normalized name → original name
	items := make([]apiv1alpha1.CatalogItem, 0, len(models))

	for _, m := range models {
		item, err := toItem(catalogName, m, seenNames)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, item)
	}

	return []apiv1alpha1.Catalog{catalog}, items, nil
}

func buildCatalog(name string) apiv1alpha1.Catalog {
	dn := name
	return apiv1alpha1.Catalog{
		ApiVersion: "flightctl.io/v1alpha1",
		Kind:       "Catalog",
		Metadata: apiv1beta1.ObjectMeta{
			Name: &name,
		},
		Spec: apiv1alpha1.CatalogSpec{
			DisplayName: &dn,
		},
	}
}

func toItem(catalogName string, m collectedModel, seenNames map[string]string) (apiv1alpha1.CatalogItem, error) {
	normalized, err := normalizeName(m.model.Name)
	if err != nil {
		id := safeID(m.model.Id)
		return apiv1alpha1.CatalogItem{}, fmt.Errorf(
			"registered model id=%s name=%q: name normalization failed: %w", id, m.model.Name, err)
	}

	if original, seen := seenNames[normalized]; seen {
		return apiv1alpha1.CatalogItem{}, fmt.Errorf(
			"name collision: models %q and %q both normalize to %q; "+
				"rename one of them to disambiguate",
			original, m.model.Name, normalized)
	}
	seenNames[normalized] = m.model.Name

	// Determine the shared OCI repository from all eligible versions.
	sharedRepo, err := sharedRepository(m)
	if err != nil {
		return apiv1alpha1.CatalogItem{}, err
	}

	// Build version list.
	versions, err := toVersions(m)
	if err != nil {
		return apiv1alpha1.CatalogItem{}, err
	}

	// Build the item spec.
	spec := apiv1alpha1.CatalogItemSpec{
		Type:     apiv1alpha1.CatalogItemTypeData,
		Category: ptr(apiv1alpha1.CatalogItemCategoryApplication),
		Artifacts: []apiv1alpha1.CatalogItemArtifact{
			{
				Type: apiv1alpha1.CatalogItemArtifactTypeContainer,
				Uri:  sharedRepo,
			},
		},
		Versions:    versions,
		DisplayName: &m.model.Name,
	}
	if m.model.Description != nil && *m.model.Description != "" {
		spec.ShortDescription = m.model.Description
	}
	if p := modelProvider(m.model); p != "" {
		spec.Provider = &p
	}

	return apiv1alpha1.CatalogItem{
		ApiVersion: "flightctl.io/v1alpha1",
		Kind:       "CatalogItem",
		Metadata: apiv1alpha1.CatalogItemMeta{
			Name:    &normalized,
			Catalog: catalogName,
		},
		Spec: spec,
	}, nil
}

// sharedRepository ensures all eligible versions of a model resolve to the
// same version-less OCI repository. Returns an error if any version diverges.
func sharedRepository(m collectedModel) (string, error) {
	if len(m.versions) == 0 {
		return "", fmt.Errorf("registered model id=%s name=%q has no eligible versions",
			safeID(m.model.Id), m.model.Name)
	}
	repo := m.versions[0].repository
	for _, v := range m.versions[1:] {
		if v.repository != repo {
			return "", fmt.Errorf(
				"registered model id=%s name=%q: version %q resolves to repository %q "+
					"but version %q resolves to repository %q; all versions of a model must "+
					"share the same OCI repository",
				safeID(m.model.Id), m.model.Name,
				m.versions[0].version.Name, repo,
				v.version.Name, v.repository)
		}
	}
	return repo, nil
}

// toVersions converts a collectedModel's versions to CatalogItemVersion entries.
func toVersions(m collectedModel) ([]apiv1alpha1.CatalogItemVersion, error) {
	versions := make([]apiv1alpha1.CatalogItemVersion, 0, len(m.versions))
	for _, v := range m.versions {
		if !isValidSemVer(v.version.Name) {
			return nil, fmt.Errorf(
				"registered model id=%s name=%q, version id=%s name=%q: "+
					"version name is not valid strict SemVer; "+
					"rename it in the Model Registry to a valid SemVer string",
				safeID(m.model.Id), m.model.Name,
				safeID(v.version.Id), v.version.Name)
		}
		versions = append(versions, apiv1alpha1.CatalogItemVersion{
			Version:  apiv1alpha1.SemVer(v.version.Name),
			Channels: []string{"stable"},
			References: map[apiv1alpha1.CatalogItemArtifactType]string{
				apiv1alpha1.CatalogItemArtifactTypeContainer: v.digest,
			},
		})
	}
	return versions, nil
}

// splitArtifactURI parses an artifact URI into a version-less repository and a
// digest. The uri may have an optional "oci://" scheme prefix which is stripped.
// The URI must contain "@sha256:" followed by exactly 64 lowercase hex chars.
//
// Returns the repository (without scheme) and the digest (sha256:…).
func splitArtifactURI(uri string) (repo, digest string, err error) {
	// Strip optional oci:// scheme.
	rawURI := strings.TrimPrefix(uri, "oci://")
	if rawURI == "" {
		return "", "", fmt.Errorf("URI is empty after stripping scheme")
	}

	// Split on "@".
	atIdx := strings.LastIndex(rawURI, "@")
	if atIdx < 0 {
		return "", "", fmt.Errorf("URI %q does not contain a digest (no '@')", uri)
	}
	candidateRepo := rawURI[:atIdx]
	candidateDigest := rawURI[atIdx+1:]

	if !ociDigestRe.MatchString(candidateDigest) {
		return "", "", fmt.Errorf(
			"URI %q: digest %q is not a valid sha256 digest (expected sha256:[a-f0-9]{64})",
			uri, candidateDigest)
	}
	if candidateRepo == "" {
		return "", "", fmt.Errorf("URI %q: repository part is empty", uri)
	}
	return candidateRepo, candidateDigest, nil
}

// isEligibleArtifact returns true when the artifact satisfies the eligibility
// criteria: artifactType==model-artifact, non-empty OCI URI with a valid
// sha256 digest, and an eligible artifact state.
//
// The eligibleArtifactStates var encodes the unresolved Open Question 9.4.
func isEligibleArtifact(a *mrapi.ModelArtifact) (repo, digest string, ok bool, err error) {
	// Check artifact type discriminator.
	if a.ArtifactType == nil || *a.ArtifactType != "model-artifact" {
		return "", "", false, nil
	}

	// Check state. nil state is treated as UNKNOWN (the default per the spec).
	var state mrapi.ArtifactState
	if a.State != nil {
		state = *a.State
	} else {
		state = mrapi.ARTIFACTSTATE_UNKNOWN
	}
	if !eligibleArtifactStates[state] {
		return "", "", false, nil
	}

	// Check URI.
	if a.Uri == nil || *a.Uri == "" {
		return "", "", false, nil
	}

	repo, digest, err = splitArtifactURI(*a.Uri)
	if err != nil {
		return "", "", false, fmt.Errorf("artifact uri: %w", err)
	}
	return repo, digest, true, nil
}

// isValidSemVer returns true if s is a valid strict SemVer string. It does not
// accept a leading 'v' prefix.
//
// Strict SemVer grammar: MAJOR.MINOR.PATCH(-pre)?(+build)? where each of
// MAJOR, MINOR, PATCH is a non-negative integer with no leading zeros.
var semverRe = regexp.MustCompile(
	`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`,
)

func isValidSemVer(s string) bool {
	return semverRe.MatchString(s)
}

func modelProvider(m mrapi.RegisteredModel) string {
	if m.Owner != nil && *m.Owner != "" {
		return *m.Owner
	}
	if m.Provider != nil && *m.Provider != "" {
		return *m.Provider
	}
	return ""
}

func safeID(id *string) string {
	if id == nil {
		return "<nil>"
	}
	return *id
}

func ptr[T any](v T) *T { return &v }

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
