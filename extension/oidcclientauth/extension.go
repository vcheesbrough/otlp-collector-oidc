package oidcclientauth

import (
	"context"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/extension/extensionauth"

	"github.com/vcheesbrough/otlp-collector-oidc/extension/oidcclientauth/internal/metadata"
)

// authenticator validates the bearer token of every request and puts the
// identity it attests into the request's client.Info.
type authenticator struct {
	provider   *provider
	verifier   *verifier
	authorizer authorizer
	rejections *rejections
	telemetry  *metadata.TelemetryBuilder

	claimAttributes    map[string]string
	resourceAttributes map[string]string

	// keys is nil until the provider has loaded; requests are not ready
	// until then.
	keys atomic.Pointer[keySet]

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var (
	_ extension.Extension  = (*authenticator)(nil)
	_ extensionauth.Server = (*authenticator)(nil)
)

// Start begins discovery in the background and returns at once: an
// unreachable provider delays nothing and fails nothing.
func (a *authenticator) Start(ctx context.Context, _ component.Host) error {
	// The discovery loop outlives Start's context and ends with Shutdown.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.cancel = cancel
	a.wg.Go(func() { a.provider.run(runCtx) })
	return nil
}

// Shutdown stops discovery, waits for it within ctx, and releases the
// telemetry.
func (a *authenticator) Shutdown(ctx context.Context) error {
	if a.cancel != nil {
		a.cancel()
	}
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	a.telemetry.Shutdown()
	return err
}

// Authenticate returns ctx carrying the token's identity, or the refusal.
// It never chooses a status: the receiver answers ErrNotReady with 503 and
// every other error with 401.
func (a *authenticator) Authenticate(ctx context.Context, sources map[string][]string) (context.Context, error) {
	token, ok := bearerToken(sources)
	if !ok {
		return ctx, a.refuse(ctx, noToken())
	}
	keys := a.keys.Load()
	if keys == nil {
		a.rejections.record(ctx, reasonNotReady, ErrNotReady)
		return ctx, ErrNotReady
	}
	c, rej := a.verifier.verify(ctx, keys, token)
	if rej != nil {
		return ctx, a.refuse(ctx, rej)
	}
	if rej := a.authorizer.authorize(c); rej != nil {
		return ctx, a.refuse(ctx, rej)
	}
	info := client.FromContext(ctx)
	info.Auth = newAuthData(c, a.claimAttributes, a.resourceAttributes)
	return client.NewContext(ctx, info), nil
}

func (a *authenticator) refuse(ctx context.Context, rej *rejectionError) error {
	a.rejections.record(ctx, rej.reason, rej)
	return rej
}
