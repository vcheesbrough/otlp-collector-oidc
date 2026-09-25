package oidcclientauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/zap"
)

// provider is the issuer's lifecycle: discovery and the first JWKS load,
// retried in the background until both succeed. Until then the authenticator
// answers not ready; the process never fails for it.
type provider struct {
	issuer string
	retry  time.Duration
	client *http.Client
	logger *zap.Logger
	// ready receives the loaded key set, once.
	ready func(*keySet)
}

// run discovers the provider and loads its keys, retrying every p.retry,
// until it succeeds or ctx ends.
func (p *provider) run(ctx context.Context) {
	for {
		keys, err := p.load(ctx)
		if err == nil {
			p.ready(keys)
			p.logger.Info("OIDC provider ready", zap.String("issuer", p.issuer), zap.Int("keys", keys.size()))
			return
		}
		if ctx.Err() != nil {
			return
		}
		p.logger.Warn("OIDC discovery failed; requests are answered not ready until it succeeds",
			zap.String("issuer", p.issuer), zap.Duration("retry", p.retry), zap.Error(err))
		timer := time.NewTimer(p.retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// load runs discovery with go-oidc, which also checks the document's issuer
// is exactly the configured one, then fetches the JWKS it names.
func (p *provider) load(ctx context.Context) (*keySet, error) {
	discovered, err := oidc.NewProvider(oidc.ClientContext(ctx, p.client), p.issuer)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := discovered.Claims(&meta); err != nil {
		return nil, fmt.Errorf("discovery document: %w", err)
	}
	if meta.JWKSURI == "" {
		return nil, errors.New("discovery document has no jwks_uri")
	}
	keys := newKeySet(p.client, meta.JWKSURI)
	if err := keys.refresh(ctx); err != nil {
		return nil, err
	}
	return keys, nil
}
