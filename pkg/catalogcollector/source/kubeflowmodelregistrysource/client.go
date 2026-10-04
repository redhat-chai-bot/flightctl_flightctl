package kubeflowmodelregistrysource

import (
	"context"

	mrapi "github.com/kubeflow/hub/pkg/openapi"
)

// registryClient abstracts the v1alpha3 Model Registry REST operations the
// source needs. Wrapping the generated client behind this interface isolates
// the source from upstream import-path churn (model-registry → hub rename) and
// the pending v1 REST API with breaking field renames.
type registryClient interface {
	// ListRegisteredModels returns all pages of registered models matching the
	// given state filter, ordered by ID ascending. Each call returns at most one
	// page; the caller drives pagination via nextPageToken.
	ListRegisteredModels(ctx context.Context, nextToken string) (*mrapi.RegisteredModelList, error)

	// ListModelVersions returns all pages of model versions for the given
	// registered model ID, matching the given state filter. Each call returns
	// at most one page.
	ListModelVersions(ctx context.Context, modelID string, nextToken string) (*mrapi.ModelVersionList, error)

	// ListModelArtifacts returns all pages of model-artifact typed artifacts for
	// the given model version ID. Each call returns at most one page.
	ListModelArtifacts(ctx context.Context, versionID string, nextToken string) (*mrapi.ArtifactList, error)
}

// openapiClient wraps the generated ModelRegistryServiceAPIService, applying
// consistent pagination parameters (pageSize, orderBy=ID, sortOrder=ASC,
// filterQuery=state='LIVE') to every list call.
type openapiClient struct {
	api      *mrapi.ModelRegistryServiceAPIService
	pageSize string
}

func (c *openapiClient) ListRegisteredModels(ctx context.Context, nextToken string) (*mrapi.RegisteredModelList, error) {
	req := c.api.GetRegisteredModels(ctx).
		PageSize(c.pageSize).
		OrderBy(mrapi.ORDERBYFIELD_ID).
		SortOrder(mrapi.SORTORDER_ASC).
		FilterQuery("state='LIVE'")
	if nextToken != "" {
		req = req.NextPageToken(nextToken)
	}
	list, _, err := req.Execute()
	return list, err
}

func (c *openapiClient) ListModelVersions(ctx context.Context, modelID string, nextToken string) (*mrapi.ModelVersionList, error) {
	req := c.api.GetRegisteredModelVersions(ctx, modelID).
		PageSize(c.pageSize).
		OrderBy(mrapi.ORDERBYFIELD_ID).
		SortOrder(mrapi.SORTORDER_ASC).
		FilterQuery("state='LIVE'")
	if nextToken != "" {
		req = req.NextPageToken(nextToken)
	}
	list, _, err := req.Execute()
	return list, err
}

func (c *openapiClient) ListModelArtifacts(ctx context.Context, versionID string, nextToken string) (*mrapi.ArtifactList, error) {
	req := c.api.GetModelVersionArtifacts(ctx, versionID).
		PageSize(c.pageSize).
		OrderBy(mrapi.ORDERBYFIELD_ID).
		SortOrder(mrapi.SORTORDER_ASC).
		ArtifactType(mrapi.ARTIFACTTYPEQUERYPARAM_MODEL_ARTIFACT)
	if nextToken != "" {
		req = req.NextPageToken(nextToken)
	}
	list, _, err := req.Execute()
	return list, err
}
