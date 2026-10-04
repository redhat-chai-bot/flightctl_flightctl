package kubeflowmodelregistrysource

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/flightctl/flightctl/internal/util"
	"github.com/flightctl/flightctl/pkg/catalogcollector/source/pollsource"
)

const (
	defaultPageSize          = 100
	defaultPollInterval      = 5 * time.Minute
	defaultRequestTimeout    = 30 * time.Second
	defaultCollectionTimeout = 5 * time.Minute
)

// AuthConfig holds a reference to the authentication extension used by this source.
type AuthConfig struct {
	// Authenticator is the component ID of the extension that provides HTTP
	// authentication (e.g. "bearertokenauth/mr"). Required.
	Authenticator string `json:"authenticator"`
}

// Config holds the provider-specific configuration for the Kubeflow Model
// Registry source. It implements config.Validator.
type Config struct {
	// Endpoint is the absolute base URL of the Model Registry REST API
	// (e.g. "https://model-registry-rhoai-rest.apps.example.com"). Required.
	Endpoint string `json:"endpoint"`

	// Catalog is the name of the target Flightctl Catalog resource. Required.
	Catalog string `json:"catalog"`

	// PollInterval is the wait between successive successful collection cycles.
	// Defaults to 5m.
	PollInterval *util.Duration `json:"pollInterval,omitempty"`

	// PageSize is the number of items requested per paginated API call.
	// Must be positive. Defaults to 100.
	PageSize int `json:"pageSize,omitempty"`

	// RequestTimeout bounds a single HTTP request. Defaults to 30s.
	RequestTimeout *util.Duration `json:"requestTimeout,omitempty"`

	// CollectionTimeout bounds the entire collection cycle (all pagination +
	// normalization). Defaults to 5m.
	CollectionTimeout *util.Duration `json:"collectionTimeout,omitempty"`

	// Auth holds the reference to the authentication extension. Required.
	Auth *AuthConfig `json:"auth,omitempty"`

	// CertificateAuthority is the path to a PEM CA bundle used to verify the
	// registry TLS certificate. Optional; system roots are used when absent.
	CertificateAuthority string `json:"certificateAuthority,omitempty"`

	// InsecureSkipVerify disables TLS verification. Development only.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// Backoff holds bounded exponential backoff parameters.
	Backoff pollsource.BackoffConfig `json:"backoff,omitempty"`
}

// Validate implements config.Validator. It returns an error if required fields
// are absent or field values are out of range.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Endpoint) == "" {
		return fmt.Errorf("missing required field \"endpoint\"")
	}
	u, err := url.ParseRequestURI(c.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("endpoint %q must be an absolute URL (e.g. https://host/path)", c.Endpoint)
	}
	if strings.TrimSpace(c.Catalog) == "" {
		return fmt.Errorf("missing required field \"catalog\"")
	}
	if c.PollInterval != nil && time.Duration(*c.PollInterval) <= 0 {
		return fmt.Errorf("pollInterval must be positive when set, got %s", time.Duration(*c.PollInterval))
	}
	if c.PageSize < 0 {
		return fmt.Errorf("pageSize must be non-negative, got %d", c.PageSize)
	}
	if c.RequestTimeout != nil && time.Duration(*c.RequestTimeout) <= 0 {
		return fmt.Errorf("requestTimeout must be positive when set, got %s", time.Duration(*c.RequestTimeout))
	}
	if c.CollectionTimeout != nil && time.Duration(*c.CollectionTimeout) <= 0 {
		return fmt.Errorf("collectionTimeout must be positive when set, got %s", time.Duration(*c.CollectionTimeout))
	}
	// Auth is optional; omitting it means requests are sent without authentication.
	if err := c.Backoff.Validate(); err != nil {
		return err
	}
	return nil
}

func (c *Config) pollInterval() time.Duration {
	if c.PollInterval != nil {
		return time.Duration(*c.PollInterval)
	}
	return defaultPollInterval
}

func (c *Config) requestTimeout() time.Duration {
	if c.RequestTimeout != nil {
		return time.Duration(*c.RequestTimeout)
	}
	return defaultRequestTimeout
}

func (c *Config) collectionTimeout() time.Duration {
	if c.CollectionTimeout != nil {
		return time.Duration(*c.CollectionTimeout)
	}
	return defaultCollectionTimeout
}

func (c *Config) pageSize() int {
	if c.PageSize > 0 {
		return c.PageSize
	}
	return defaultPageSize
}
