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
// retried in the background until both succeed, then a JWKS refresh on a
// fixed interval so a key the provider removes stops being trusted. Until the
// first load the authenticator answers not ready; the process never fails
// for it.
type provider struct {
	issuer string
	retry  time.Duration
	// refresh is the interval of the scheduled JWKS refresh.
	refresh time.Duration
	client  *http.Client
	logger  *zap.Logger
	now     func() time.Time
	// ready receives the loaded key set, once.
	ready func(*keySet)
}

// run discovers the provider, hands its keys over, and keeps them fresh until
// ctx ends.
func (p *provider) run(ctx context.Context) {
	keys := p.discover(ctx)
	if keys == nil {
		return
	}
	p.ready(keys)
	p.logger.Info("OIDC provider ready", zap.String("issuer", p.issuer), zap.Int("keys", keys.size()))
	p.keepFresh(ctx, keys)
}

// discover loads the provider, retrying every p.retry, until it succeeds or
// ctx ends; it returns nil only when ctx ends.
func (p *provider) discover(ctx context.Context) *keySet {
	for {
		keys, err := p.load(ctx)
		if err == nil {
			return keys
		}
		if ctx.Err() != nil {
			return nil
		}
		p.logger.Warn("OIDC discovery failed; requests are answered not ready until it succeeds",
			zap.String("issuer", p.issuer), zap.Duration("retry", p.retry), zap.Error(err))
		if !sleep(ctx, p.retry) {
			return nil
		}
	}
}

// keepFresh refreshes the JWKS every p.refresh until ctx ends. A failed
// refresh keeps the keys already loaded, so a provider outage refuses no
// token it would have accepted.
func (p *provider) keepFresh(ctx context.Context, keys *keySet) {
	for sleep(ctx, p.refresh) {
		if err := keys.refresh(ctx); err != nil && ctx.Err() == nil {
			p.logger.Warn("JWKS refresh failed; keeping the cached keys",
				zap.String("issuer", p.issuer), zap.Duration("retry", p.refresh), zap.Error(err))
		}
	}
}

// sleep waits for d and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
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
	keys := newKeySet(p.client, meta.JWKSURI, p.now)
	if err := keys.refresh(ctx); err != nil {
		return nil, err
	}
	return keys, nil
}
