package kubeflowmodelregistrysource

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	mrapi "github.com/kubeflow/hub/pkg/openapi"

	catalogcollector "github.com/flightctl/flightctl/pkg/catalogcollector"
	"github.com/flightctl/flightctl/pkg/catalogcollector/config"
	"github.com/flightctl/flightctl/pkg/catalogcollector/extension/extensionauth"
	"github.com/flightctl/flightctl/pkg/catalogcollector/source/pollsource"
)

// Type is the component type for the Kubeflow Model Registry source.
const Type catalogcollector.ComponentType = "kubeflowmodelregistry"

type factory struct{}

// NewFactory returns a SourceFactory for the Kubeflow Model Registry source.
func NewFactory() catalogcollector.SourceFactory {
	return &factory{}
}

func (f *factory) Type() catalogcollector.ComponentType {
	return Type
}

func (f *factory) CreateDefaultConfig() catalogcollector.ComponentConfig {
	return &Config{
		Backoff: pollsource.DefaultBackoffConfig(),
	}
}

func (f *factory) CreateSource(
	_ context.Context,
	settings catalogcollector.Settings,
	cfg catalogcollector.ComponentConfig,
	next catalogcollector.Consumer,
) (catalogcollector.Source, error) {
	c, ok := cfg.(*Config)
	if !ok {
		return nil, fmt.Errorf("source %q: unexpected config type %T", settings.ID, cfg)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("source %q: %w", settings.ID, err)
	}

	// Build the TLS transport.
	baseTransport, err := buildTransport(c)
	if err != nil {
		return nil, fmt.Errorf("source %q: building TLS transport: %w", settings.ID, err)
	}

	// Wrap the base transport with the auth RoundTripper when auth is configured.
	// Auth is optional; when absent the base transport is used as-is.
	authedTransport := http.RoundTripper(baseTransport)
	if c.Auth != nil && strings.TrimSpace(c.Auth.Authenticator) != "" {
		authID, err := catalogcollector.ParseComponentID(c.Auth.Authenticator)
		if err != nil {
			return nil, fmt.Errorf("source %q: auth.authenticator %q is not a valid component ID: %w",
				settings.ID, c.Auth.Authenticator, err)
		}
		ext, err := settings.Host.GetExtension(authID)
		if err != nil {
			return nil, fmt.Errorf("source %q: resolving auth extension %q: %w", settings.ID, authID, err)
		}
		authClient, ok := ext.(extensionauth.HTTPClient)
		if !ok {
			return nil, fmt.Errorf("source %q: extension %q does not implement extensionauth.HTTPClient",
				settings.ID, authID)
		}
		authedTransport, err = authClient.RoundTripper(baseTransport)
		if err != nil {
			return nil, fmt.Errorf("source %q: building auth transport: %w", settings.ID, err)
		}
	}

	// Build the openapi client with the fully composed transport.
	apiCfg := mrapi.NewConfiguration()
	// The SDK-generated client already prepends /api/model_registry/v1alpha3/ to
	// every request path, so the server URL must be just the base endpoint.
	apiCfg.Servers = mrapi.ServerConfigurations{{
		URL: c.Endpoint,
	}}
	apiCfg.HTTPClient = &http.Client{
		Transport: authedTransport,
		Timeout:   c.requestTimeout(),
	}

	client := &openapiClient{
		api:      mrapi.NewAPIClient(apiCfg).ModelRegistryServiceAPI,
		pageSize: fmt.Sprintf("%d", c.pageSize()),
	}

	// Build the metrics instruments.
	metrics, err := newMetrics(settings.ID.String(), settings.MeterProvider)
	if err != nil {
		return nil, fmt.Errorf("source %q: creating metrics: %w", settings.ID, err)
	}

	// Build the poll helper.
	poller := pollsource.NewHelper(
		settings.ID.String(),
		c.pollInterval(),
		c.Backoff,
		settings.Logger,
		nil, // use production time.Now
		nil, // use production rand jitter
	)
	poller.OnSuccess = metrics.recordSuccess
	poller.OnFailure = metrics.recordFailure

	return &source{
		id:                settings.ID.String(),
		catalog:           c.Catalog,
		collectionTimeout: c.collectionTimeout(),
		client:            client,
		poller:            poller,
		next:              next,
		log:               settings.Logger,
		metrics:           metrics,
	}, nil
}

// buildTransport constructs a TLS-aware base HTTP transport for the source.
// CA trust is configured separately from the authentication wrapper.
func buildTransport(c *Config) (http.RoundTripper, error) {
	tlsCfg := &tls.Config{
		InsecureSkipVerify: c.InsecureSkipVerify, //nolint:gosec // operator-controlled dev flag
	}

	if c.CertificateAuthority != "" {
		pem, err := os.ReadFile(c.CertificateAuthority)
		if err != nil {
			return nil, fmt.Errorf("reading certificateAuthority %q: %w", c.CertificateAuthority, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("certificateAuthority %q contained no valid certificates", c.CertificateAuthority)
		}
		tlsCfg.RootCAs = pool
	}

	return &http.Transport{
		TLSClientConfig:    tlsCfg,
		MaxIdleConns:       10,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: false,
		ForceAttemptHTTP2:  true,
	}, nil
}

// Ensure config.Validator is satisfied at compile time.
var _ config.Validator = (*Config)(nil)
