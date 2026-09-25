// Package oidcclientauth is a server authenticator that accepts a request
// only with a JWT access token, from the configured OIDC provider, as an
// Authorization: Bearer header. docs/token-profile.md is the contract it
// enforces; every refusal's text names the rule the token broke.
//
// go-oidc is used for discovery only. Its IDTokenVerifier is built for ID
// tokens, and its RemoteKeySet neither tells an unknown kid from a bad
// signature nor reports when its keys have loaded, so the key set and the
// access-token rules are here, on go-jose, which go-oidc itself uses. The
// provider is ready once discovery has succeeded and the JWKS has loaded;
// until then Authenticate returns ErrNotReady.
//
//go:generate go tool mdatagen metadata.yaml
package oidcclientauth
