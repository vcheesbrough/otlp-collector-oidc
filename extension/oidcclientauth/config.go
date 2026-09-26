package oidcclientauth

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"go.opentelemetry.io/collector/component"
)

// Defaults, as DESIGN §6.2 states them.
const (
	defaultDiscoveryRetry       = 30 * time.Second
	defaultJWKSRefresh          = 10 * time.Minute
	defaultRequiredScope        = "telemetry:write"
	defaultClockSkew            = 60 * time.Second
	defaultRejectionLogInterval = 60 * time.Second
)

// supportedAlgorithms is every alg the token profile may allow: RSA PKCS#1
// v1.5 and ECDSA. none, HMAC and anything else are never accepted, whatever
// the configuration says.
func supportedAlgorithms() []jose.SignatureAlgorithm {
	return []jose.SignatureAlgorithm{
		jose.RS256, jose.RS384, jose.RS512,
		jose.ES256, jose.ES384, jose.ES512,
	}
}

// Config is the YAML shape of the authenticator.
type Config struct {
	// IssuerURL is the exact iss value; discovery reads
	// <issuer_url>/.well-known/openid-configuration.
	IssuerURL string `mapstructure:"issuer_url"`
	// Audience is the value aud must contain, normally the client id.
	Audience string `mapstructure:"audience"`
	// DiscoveryRetry is how long to wait between failed discovery attempts.
	DiscoveryRetry time.Duration `mapstructure:"discovery_retry"`
	// JWKSRefresh is how often the JWKS is re-read once loaded: the longest a
	// key the provider removes is still trusted.
	JWKSRefresh time.Duration `mapstructure:"jwks_refresh"`
	// RequiredScope must appear in scope (or scp).
	RequiredScope string `mapstructure:"required_scope"`
	// RequiredClaims must each be present and non-empty.
	RequiredClaims []string `mapstructure:"required_claims"`
	// ClaimAttributes maps a claim to the auth-context attribute it becomes.
	// A claim that is absent, or not a string, is skipped.
	ClaimAttributes map[string]string `mapstructure:"claim_attributes"`
	// ResourceAttributes are static attributes the deployment states; each
	// is added to the auth context of every authenticated request.
	ResourceAttributes map[string]string `mapstructure:"resource_attributes"`
	// ClockSkew is the tolerance on exp, nbf and iat.
	ClockSkew time.Duration `mapstructure:"clock_skew"`
	// RejectionLogInterval is the least time between two warnings for the
	// same reason.
	RejectionLogInterval time.Duration `mapstructure:"rejection_log_interval"`
	// Algorithms is the alg allowlist, a subset of RS256/384/512 and
	// ES256/384/512.
	Algorithms []string `mapstructure:"algorithms"`

	// prevent unkeyed literal initialization
	_ struct{}
}

var _ component.Config = (*Config)(nil)

// Errors Validate returns, joined, for every setting it refuses.
var (
	ErrNoIssuer              = errors.New("issuer_url must be set")
	ErrIssuerNotURL          = errors.New("issuer_url must be an absolute http or https URL")
	ErrNoAudience            = errors.New("audience must be set")
	ErrDiscoveryRetry        = errors.New("discovery_retry must be positive")
	ErrJWKSRefresh           = errors.New("jwks_refresh must be positive")
	ErrRequiredScope         = errors.New("required_scope must be one non-empty scope token")
	ErrEmptyRequiredClaim    = errors.New("required_claims must not contain an empty name")
	ErrEmptyClaimAttribute   = errors.New("claim_attributes must not contain an empty claim or attribute")
	ErrEmptyResourceKey      = errors.New("resource_attributes must not contain an empty key")
	ErrAttributeCollision    = errors.New("an attribute is named by both claim_attributes and resource_attributes")
	ErrClockSkew             = errors.New("clock_skew must not be negative")
	ErrRejectionLogInterval  = errors.New("rejection_log_interval must be positive")
	ErrNoAlgorithms          = errors.New("algorithms must name at least one algorithm")
	ErrUnsupportedAlgorithms = errors.New("algorithms may contain only RS256, RS384, RS512, ES256, ES384, ES512")
)

// Validate fails fast on a configuration the authenticator cannot enforce.
func (c *Config) Validate() error {
	var errs []error
	switch u, err := url.Parse(c.IssuerURL); {
	case c.IssuerURL == "":
		errs = append(errs, ErrNoIssuer)
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		errs = append(errs, ErrIssuerNotURL)
	}
	if c.Audience == "" {
		errs = append(errs, ErrNoAudience)
	}
	if c.DiscoveryRetry <= 0 {
		errs = append(errs, ErrDiscoveryRetry)
	}
	if c.JWKSRefresh <= 0 {
		errs = append(errs, ErrJWKSRefresh)
	}
	if c.RequiredScope == "" || strings.ContainsAny(c.RequiredScope, " \t\r\n") {
		errs = append(errs, ErrRequiredScope)
	}
	for _, name := range c.RequiredClaims {
		if name == "" {
			errs = append(errs, ErrEmptyRequiredClaim)
			break
		}
	}
	for claim, attr := range c.ClaimAttributes {
		if claim == "" || attr == "" {
			errs = append(errs, ErrEmptyClaimAttribute)
			break
		}
	}
	for key := range c.ResourceAttributes {
		if key == "" {
			errs = append(errs, ErrEmptyResourceKey)
			break
		}
	}
	for _, attr := range c.ClaimAttributes {
		if _, ok := c.ResourceAttributes[attr]; ok {
			errs = append(errs, fmt.Errorf("%w: %s", ErrAttributeCollision, attr))
		}
	}
	if c.ClockSkew < 0 {
		errs = append(errs, ErrClockSkew)
	}
	if c.RejectionLogInterval <= 0 {
		errs = append(errs, ErrRejectionLogInterval)
	}
	if _, err := c.algorithms(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// algorithms is the allowlist as go-jose types.
func (c *Config) algorithms() ([]jose.SignatureAlgorithm, error) {
	if len(c.Algorithms) == 0 {
		return nil, ErrNoAlgorithms
	}
	out := make([]jose.SignatureAlgorithm, 0, len(c.Algorithms))
	for _, name := range c.Algorithms {
		alg := jose.SignatureAlgorithm(name)
		if !slices.Contains(supportedAlgorithms(), alg) {
			return nil, fmt.Errorf("%w: %q", ErrUnsupportedAlgorithms, name)
		}
		out = append(out, alg)
	}
	return out, nil
}

func createDefaultConfig() component.Config {
	supported := supportedAlgorithms()
	algs := make([]string, len(supported))
	for i, a := range supported {
		algs[i] = string(a)
	}
	return &Config{
		DiscoveryRetry: defaultDiscoveryRetry,
		JWKSRefresh:    defaultJWKSRefresh,
		RequiredScope:  defaultRequiredScope,
		RequiredClaims: []string{"sub", "preferred_username"},
		ClaimAttributes: map[string]string{
			"sub":                "user.id",
			"preferred_username": "user.name",
			"email":              "user.email",
			"name":               "user.full_name",
		},
		ClockSkew:            defaultClockSkew,
		RejectionLogInterval: defaultRejectionLogInterval,
		Algorithms:           algs,
	}
}
