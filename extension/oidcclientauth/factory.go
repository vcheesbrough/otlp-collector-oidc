package oidcclientauth

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"

	"github.com/vcheesbrough/otlp-collector-oidc/extension/oidcclientauth/internal/metadata"
)

// fetchTimeout bounds one discovery or JWKS request.
const fetchTimeout = 10 * time.Second

// NewFactory returns the factory for the oidcclientauth extension.
func NewFactory() extension.Factory {
	return extension.NewFactory(metadata.Type, createDefaultConfig, create, metadata.ExtensionStability)
}

func create(_ context.Context, set extension.Settings, cfg component.Config) (extension.Extension, error) {
	return newAuthenticator(set, cfg.(*Config), &http.Client{Timeout: fetchTimeout}, time.Now)
}

// newAuthenticator wires the authenticator from its configuration, with the
// HTTP client and clock injected.
func newAuthenticator(set extension.Settings, cfg *Config, httpClient *http.Client, now func() time.Time) (*authenticator, error) {
	algorithms, err := cfg.algorithms()
	if err != nil {
		return nil, err
	}
	tb, err := metadata.NewTelemetryBuilder(set.TelemetrySettings)
	if err != nil {
		return nil, err
	}
	a := &authenticator{
		verifier: &verifier{
			issuer:     cfg.IssuerURL,
			audience:   cfg.Audience,
			algorithms: algorithms,
			skew:       cfg.ClockSkew,
			now:        now,
		},
		authorizer:         authorizer{scope: cfg.RequiredScope, claims: cfg.RequiredClaims},
		rejections:         newRejections(tb.OidcclientauthRejections, set.Logger, cfg.RejectionLogInterval, now),
		telemetry:          tb,
		claimAttributes:    cfg.ClaimAttributes,
		resourceAttributes: cfg.ResourceAttributes,
	}
	a.provider = &provider{
		issuer:  cfg.IssuerURL,
		retry:   cfg.DiscoveryRetry,
		refresh: cfg.JWKSRefresh,
		client:  httpClient,
		logger:  set.Logger,
		now:     now,
		ready:   a.keys.Store,
	}
	return a, nil
}
