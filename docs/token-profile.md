# Token profile

This is the contract between `otlp-collector-oidc` and any OIDC provider: what a
client must present for a request to be accepted. It is normative. Its conformance
suite is the integration scenario table in
[`integration/auth_test.go`](../integration/auth_test.go), one scenario per rule,
each asserting the exact answer below over both OTLP/HTTP and OTLP/gRPC.

The key words **must**, **must not** and **may** are used as in RFC 2119.

## The token

A JWT **access token** in the shape of RFC 9068, signed, in JWS compact
serialisation. An ID token is not accepted: it carries no scope. An opaque access
token is not accepted: nothing here calls the provider's introspection endpoint.

## Delivery

The token **must** arrive as the request's one `Authorization` header with the
`Bearer` scheme (case-insensitive), as HTTP header or gRPC metadata:

```
Authorization: Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6...
```

Nothing else is read. A token in a cookie, in a query parameter, under another
scheme, or in a second `Authorization` header is not a token.

## Discovery and keys

The collector reads `<OIDC_ISSUER_URL>/.well-known/openid-configuration`, which
**must** state `issuer` exactly equal to `OIDC_ISSUER_URL` and a `jwks_uri`, then
loads that JWKS. Until both have succeeded it is not ready (below); it retries every
`OIDC_DISCOVERY_RETRY` and never exits for it.

Keys are cached, and the JWKS is re-read every `OIDC_JWKS_REFRESH` (default 10
minutes); a failed re-read keeps the cached keys.

- **Rotating a key in.** A token whose `kid` is not in the cache makes the collector
  fetch the JWKS again, once, before refusing it — at most once every 10 seconds, so
  a stream of made-up `kid`s cannot turn every request into a request to the
  provider. A provider publishes a new key before signing with it; a token signed
  with a key published less than 10 seconds after the previous such fetch may be
  refused `unknown kid` until the window passes or the scheduled re-read finds it.
- **Rotating a key out.** A key removed from the JWKS stops being trusted at the
  next scheduled re-read: within `OIDC_JWKS_REFRESH`. That is the longest a revoked
  signing key keeps working.

Keys marked `"use": "enc"`, private keys and key types the collector does not know
are ignored.

## Rules

Rules are applied in this order; the first broken one is the answer.

| # | Rule | Refusal |
| --- | --- | --- |
| 1 | An `Authorization: Bearer` header with a non-empty token | `no token` |
| 2 | Three base64url segments, the first a JSON header with `alg` | `invalid token: malformed` |
| 3 | `alg` is one of `RS256`, `RS384`, `RS512`, `ES256`, `ES384`, `ES512`. `none`, `HS*`, `PS*` and anything else are refused whatever key they name | `invalid token: unsupported alg` |
| 4 | `kid` names a key in the JWKS, after at most one refresh. A token without `kid` names none | `invalid token: unknown kid` |
| 5 | The signature verifies with that key | `invalid token: bad signature` |
| 6 | The payload is a JSON object, and `exp`, `nbf`, `iat` are numbers where present | `invalid token: malformed` |
| 7 | `iss` is exactly `OIDC_ISSUER_URL`, byte for byte (a trailing `/` matters) | `invalid token: wrong iss` |
| 8 | `aud`, a string or an array of strings, contains `OIDC_AUDIENCE` | `invalid token: wrong aud` |
| 9 | `exp` is present | `invalid token: missing exp` |
| 10 | Now is before `exp` + `CLOCK_SKEW` | `invalid token: expired` |
| 11 | If `nbf` is present, now + `CLOCK_SKEW` is not before it | `invalid token: not yet valid` |
| 12 | If `iat` is present, it is not after now + `CLOCK_SKEW` | `invalid token: issued in the future` |
| 13 | `scope`, a space-delimited string, contains `REQUIRED_SCOPE`; or `scp`, an array of strings or a space-delimited string, does | `missing scope: <REQUIRED_SCOPE>` |
| 14 | Each required claim — `sub`, then `preferred_username` — is present and not empty (not `null`, `""`, `[]` or `{}`) | `missing claim: <name>` |

With the defaults, rule 13's answer is `missing scope: telemetry:write`.

## Identity

An accepted token's identity is attached to the request for the pipeline:

| Claim | Attribute | |
| --- | --- | --- |
| `sub` | `user.id` | required |
| `preferred_username` | `user.name` | required |
| `email` | `user.email` | when present and a non-empty string |
| `name` | `user.full_name` | when present and a non-empty string |

Any other claim is ignored.

## Answers

Every refusal of a token is **401**, and the body is the refusal text from the table.

- **OTLP/HTTP:** `401` with `WWW-Authenticate: Bearer` and an OTLP `Status` body in
  the request's encoding — `{"code":16,"message":"missing scope: telemetry:write"}`
  for JSON, its protobuf equivalent for protobuf; plain text for any other content
  type.
- **OTLP/gRPC:** status `Unauthenticated` (16) with the refusal text as its message.

One answer is not a refusal: **`not ready`**, while discovery or the first JWKS load
has not succeeded. It is **503** with `Retry-After: 5` over HTTP and `Unavailable`
(14) with a `RetryInfo` of 5 seconds over gRPC, in both cases carrying the text
`not ready`. It is the only answer a client should retry rather than fix. A request
without a token is still `no token` while not ready.

A misconfigured provider therefore diagnoses itself with one request:

```sh
curl -sk https://collector:4318/v1/traces -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{}'
# {"code":16,"message":"invalid token: wrong aud"}
```
