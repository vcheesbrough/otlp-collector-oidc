# Provider guides

One page per identity provider, saying how to make it mint access tokens the
collector accepts. The collector works with any provider that meets [the token
profile](../token-profile.md); these pages exist so a deployer does not have to work
out the provider's side alone.

- [authentik](authentik.md), with a blueprint.

## Writing a page for another provider

The token profile is the contract; a provider page never restates it. It says, for
that provider, how to produce what the profile asks for and nothing else:

1. **Name the version tested.** Provider behaviour changes between releases.
2. **An access token that is a signed JWT.** Many providers issue opaque access
   tokens by default, or sign with a shared secret (HS256); say which setting gives an
   RS or ES signed JWT access token.
3. **The issuer.** Where the discovery document is, and the exact `issuer` it states,
   trailing slash included: that is `OIDC_ISSUER_URL`.
4. **The audience.** What the provider puts in `aud`, usually the client id or an API
   identifier: that is `OIDC_AUDIENCE`.
5. **The scope.** How to grant `telemetry:write` (or whatever `REQUIRED_SCOPE` is set
   to), and whether the token carries it as `scope` (a string) or `scp` (an array).
6. **The claims.** Which scopes put `preferred_username`, `email` and `name` in the
   access token, not only the ID token or userinfo; and how stable `sub` is.
7. **Who may send.** The provider's way to limit who gets a token for this client.
8. **Verify.** A recipe that gets a token the way a client does, and the collector's
   answer to it; point at the token profile for every other answer.

Prefer something reproducible (a blueprint, a Terraform module, an export) over
screenshots of a console.
