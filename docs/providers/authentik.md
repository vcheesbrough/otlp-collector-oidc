# authentik

How to make [authentik](https://goauthentik.io) mint access tokens that
`otlp-collector-oidc` accepts. What "accepts" means is [the token
profile](../token-profile.md); this page only says how to get there. Tested with
authentik **2026.8.3**.

## What you create

The blueprint [`authentik/otlp-collector-oidc.yaml`](authentik/otlp-collector-oidc.yaml)
creates everything below, so the setup is the same every time:

- **A `telemetry:write` scope mapping.** It adds no claim; being granted the scope is
  the permission, and the collector refuses a token without it (`REQUIRED_SCOPE`).
- **A public OAuth2/OpenID provider** — a browser or mobile client cannot keep a
  secret, so it uses PKCE — with:
  - `authorization_code` and `refresh_token` as its grant types, declared rather than
    left to defaults;
  - a **signing key**. It is what makes the access token a JWT signed with RS256.
    Without one, authentik signs with the client secret (HS256), and the collector
    refuses the token as `invalid token: unsupported alg`;
  - `sub_mode: hashed_user_id`. `sub` becomes `user.id` on every span. It is stable
    across edits to the provider (name, validity, signing key) and reveals nothing.
    **Changing `sub_mode` later changes every user's id**; pick it once;
  - the `openid`, `profile`, `email`, `offline_access` and `telemetry:write` scopes.
    `profile` supplies `preferred_username` and `name`, `email` supplies `email`; all
    three appear in the access token;
- **An application** whose slug fixes the issuer:
  `https://<authentik>/application/o/<slug>/`;
- **A group bound to the application.** Its members may sign in to the application,
  and so get a token; everyone else is refused at sign-in. Adding or removing a user
  is the "who may send" switch.

## Apply the blueprint

The blueprint takes its values from its context, so one file serves every product
environment. Each environment's instance must set its own `app_slug`, `app_name`,
`client_id` and `group_name`: a provider belongs to one application, so two instances
sharing a name would fight over it.

| Context key | Example | Becomes |
| --- | --- | --- |
| `app_slug` | `telemetry-prod` | the issuer path, and so `OIDC_ISSUER_URL`; never change it once clients hold tokens |
| `app_name` | `Telemetry (prod)` | the provider's and the application's name |
| `client_id` | `telemetry-prod` | the token's `aud`, and so `OIDC_AUDIENCE` |
| `redirect_uri` | `https://app.example.com/oauth/callback` | where your client receives the code |
| `group_name` | `telemetry-prod senders` | the group whose members may send |
| `signing_key_name` | `telemetry-prod signing` | the key that signs tokens |

1. Generate a dedicated signing key: **System → Certificates → Generate**, RSA, named
   as `signing_key_name`. (The default `authentik Self-signed Certificate` works, but
   is shared with everything else.)
2. Copy the blueprint to the worker's blueprint directory, for example
   `/blueprints/custom/otlp-collector-oidc.yaml`.
3. **Customisation → Blueprints → Create**: choose the file, set the context above as
   YAML, save, and **Apply**. The status becomes `successful`.
4. Add the users who may send to the group.

## Configure the collector

```sh
OIDC_ISSUER_URL=https://auth.example.com/application/o/telemetry-prod/
OIDC_AUDIENCE=telemetry-prod
REQUIRED_SCOPE=telemetry:write   # the default
```

`OIDC_ISSUER_URL` keeps its trailing slash: it must equal the `issuer` in
`https://auth.example.com/application/o/telemetry-prod/.well-known/openid-configuration`
exactly. The default `REQUIRED_CLAIMS` (`sub`, `preferred_username`) and
`CLAIM_ATTRIBUTES` (`sub`, `preferred_username`, `email`, `name`) match what the
provider puts in the token.

## Verify

Get a token as a client would, with the authorization-code flow and PKCE. In a shell:

```sh
AUTH=https://auth.example.com
CLIENT=telemetry-prod
REDIRECT=https://app.example.com/oauth/callback
VERIFIER=$(openssl rand -base64 48 | tr -d '=+/\n' | cut -c1-64)
CHALLENGE=$(printf %s "$VERIFIER" | openssl dgst -sha256 -binary | base64 | tr '+/' '-_' | tr -d '=')
echo "$AUTH/application/o/authorize/?response_type=code&client_id=$CLIENT&redirect_uri=$REDIRECT&scope=openid%20profile%20email%20telemetry%3Awrite&state=x&code_challenge=$CHALLENGE&code_challenge_method=S256"
```

Open the printed URL in a browser, sign in as a member of the group, and copy the
`code` parameter from the address the browser is sent to (the page itself may not
load). Then:

```sh
CODE=<the code>
TOKEN=$(curl -s "$AUTH/application/o/token/" -d grant_type=authorization_code \
  -d client_id=$CLIENT -d redirect_uri=$REDIRECT -d code=$CODE -d code_verifier=$VERIFIER |
  sed -n 's/.*"access_token": *"\([^"]*\)".*/\1/p')
```

Each answer below is the collector's, over OTLP/HTTP; the token profile states every
one. `-k` accepts the image's embedded certificate.

```sh
COLLECTOR=https://collector.example.com
SPAN='{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"hello","startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}'

curl -sk $COLLECTOR/v1/traces -H 'Content-Type: application/json' -d "$SPAN"
# {"code":16,"message":"no token"}

curl -sk $COLLECTOR/v1/traces -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$SPAN"
# {"partialSuccess":{}}
```

Then check the span upstream: it carries `user.id` (the hashed `sub`), `user.name`,
`user.email` and `user.full_name`, and your `CLIENT_RESOURCE_ATTRIBUTES` on its
resource. What the other answers mean, and what to change:

| Answer | Cause in authentik |
| --- | --- |
| `503` `not ready` | the collector cannot load `…/.well-known/openid-configuration` or the JWKS — or `OIDC_ISSUER_URL` differs from the discovery `issuer` (the trailing slash, the slug), which the collector refuses as a discovery failure |
| `invalid token: unsupported alg` | the provider has no signing key, so the token is HS256 |
| `invalid token: wrong iss` | the token is from another application's provider |
| `invalid token: wrong aud` | `OIDC_AUDIENCE` is not the provider's client id |
| `missing scope: telemetry:write` | the client did not request the scope, the mapping is not on the provider, or an ID token was sent instead of the access token |
| `missing claim: preferred_username` | the `profile` scope was not requested |
| `invalid token: malformed` | a refresh token or another opaque string was sent instead of the access token |
| sign-in says *Permission denied* | the user is not in the group |

Access tokens last `access_token_validity` (10 minutes in the blueprint); clients
renew them with the refresh token, which `offline_access` allows.
