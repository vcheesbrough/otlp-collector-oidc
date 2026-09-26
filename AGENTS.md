# Agent guide — otlp-collector-oidc

This file holds the **otlp-collector-oidc-specific** working rules. The machine-global
rules common to every repo — Kanban/bored workflow, iteration & versioning defaults,
branching & git safety, CI-after-push, PR self-review + comment loop, test coverage,
and MCP/secrets discipline — live in the shared **agent-shared baseline**, imported
on this machine via `~/.codex/AGENTS.md` and `~/.claude/CLAUDE.md` (run
`agent-shared/install.sh` once per machine to wire this up). **Read that baseline
first;** this file only records what is specific to this repo or overrides the
baseline.

If you change an otlp-collector-oidc rule, change it here — there is no parallel copy.
Cross-repo rules change in `agent-shared`, not here.

---

## Repository context

| Item | Value |
| --- | --- |
| **Remote / `gh` repo** | `vcheesbrough/otlp-collector-oidc` (public) |
| **Trunk** | `main` |
| **Kanban board** | `https://bored.desync.link/boards/otlp-collector-oidc` |
| **Phase** | pre-MVP `0.N.P` — iteration 1 starts at `0.1.0` |
| **CI** | **GitHub Actions**, not Woodpecker — baseline §4's GitHub-native path applies, but Actions reports **check runs**, not commit statuses: watch `gh pr checks <PR> --watch`, or read `gh api repos/vcheesbrough/otlp-collector-oidc/commits/$SHA/check-runs --jq '.check_runs[] \| [.name, .status, .conclusion]'`. Workflows: `ci.yml` (Lint, Test, Image, Badges on `main`), `release.yml` |
| **Image** | `ghcr.io/vcheesbrough/otlp-collector-oidc` (multi-arch, published by `release.yml` only after CI passes on the same commit: `main` → `:edge`, an iteration merge also → `:0.N.0` with its tag created automatically, a pushed `v*` tag → that version) |
| **Language** | Go — a custom OpenTelemetry Collector distribution built with `ocb` |

Shared skills apply here once the machine is wired up (`start-iteration`,
`ci-watch`, `pr-review-loop`). Their per-repo parameters:

- `OWNER=vcheesbrough`, `REPO=otlp-collector-oidc`
- CI reproduce commands: `make lint`, `make vet`, `make vuln`, `make test`,
  `make integration` (`go test ./integration`), `make generate-check`,
  `make versions-check`, `make docs-check`, `make build`, and
  `make image && scripts/image-smoke.sh ghcr.io/vcheesbrough/otlp-collector-oidc:dev "$(scripts/version.sh)"`
- Extra review criteria beyond the baseline five (correctness, security/OWASP, tests,
  versioning, scope): every change to `extension/` or `receiver/` is on the trust
  boundary — the reviewer checks that no client-supplied value can reach a metric
  label, that every refusal names its reason and is counted, and that nothing in the
  metrics pipeline references an `auth.` identity key. The reviewer also checks that
  the card's **SOLID section is honoured** (or the deviation is written on the card),
  the diff against the Go standards below, and that the **Definition of Done** is met.

---

## Repo-specific rules

### The design document is the specification

[`docs/DESIGN.md`](docs/DESIGN.md) is what the product does; a change to behaviour is
a change to that document in the same PR. [`docs/decisions/`](docs/decisions/) records
why. [`docs/reference-deployment.md`](docs/reference-deployment.md) is the
maintainer's own deployment and is an example, never a requirement — **nothing in
the product may depend on Traefik, authentik, sovereign-config or Grafana Alloy.**

### Versioning

Baseline §2 unchanged: the workspace version is the Go module's release tag
(`v0.N.P`), one iteration = one semver minor, `1.0.0` is the MVP. The MVP bar is the
`observability` skill's §1 applied to this product itself: it exports its own logs
over OTLP and its own metrics on `:8888`, ships a dashboard for them, and is never
in a health gate.

**Releases are cut by the merge.** When an iteration's PR — branch
`feat/iteration-N-<slug>`, as `start-iteration` names it — merges and CI passes on
`main`, `release.yml` tags the merge commit `vM.N.0` (M the current major) and
publishes `:M.N.0` beside `:edge`. Nobody tags an iteration by hand, and the branch
name is what marks the merge as a release, so it must follow the convention: N must
be exactly the next iteration (a wrong N fails the release run and publishes
nothing — push the right tag by hand), and only a branch of this repository
counts, never a fork's. Other
merges (fixes, Dependabot, `ci/…` branches) publish `:edge` only. A patch release is
a hand-pushed `vX.Y.P` tag; CI runs on it and `release.yml` publishes it. The MVP is
the one hand-cut tag: after the MVP card's merge has been released as `v0.N.0`, push
`v1.0.0` on the same commit. The rules are `scripts/release-plan.sh`, tested by
`make release-plan-test`.

### Integration tests are the primary tier

They carry the majority of coverage. The system under test is the built binary as a
subprocess, driven from outside over HTTP and gRPC on its one port, observed at fake
sinks, on `:8888`, on the health endpoint and on its output (`integration/harness`).
Every behaviour a card adds lands as scenarios in `integration/`, run over **both**
protocols. Unit tests are written only for what cannot be driven or observed from
outside the process, and each such test says why. Renderer golden files are a fast
secondary check, never the acceptance. This overrides baseline §6's "lowest
responsible layer" for this repo. The suite's budget is **five minutes** on a
GitHub-hosted runner; exceeding it is a finding to fix (one subprocess per
environment shape, parallel scenarios), not a limit to raise.

### The README is a product surface

It is public-facing and its quality matters as much as the software's: terse,
professional, no design-stage chatter, no internal process. Every card that changes
behaviour updates it in the same PR, keeping its structure — title and tagline; badge
row; what it does, what it refuses, what it assumes nothing about; Quick start;
Configuration; Deploy; Observability; Development; Status; Licence. The structure
changes only with a reason recorded on the card. Badges stay green on `main`; a red
badge is an incident, not a backlog item.

### SOLID is evaluated per card

Every card carries a `## SOLID` section stating how the code it adds is split and
what it depends on. The PR self-review evaluates the diff against that section and
the standards below; a deviation is recorded on the card, not left silent.

### Definition of Done

A card's PR is ready to merge only when all of these hold, and the self-review checks
each: every behaviour the card adds has integration scenarios over both protocols;
the README section it touches is updated in place; `docs/configuration.md` is
regenerated (`make docs`) if a variable changed, which `make docs-check` enforces; the observability decision is recorded
on the card, naming the dashboard panel, alert rule and runbook entry added (in
`dashboards/`, `alerts/`, `docs/runbook.md`) or "none" with a reason, and a new metric
the artefacts query is created in `TestArtefactMetrics`' shape;
the card's SOLID section is honoured or the deviation is written on the card;
DESIGN.md is updated where behaviour or a §10 finding changed; CI is green and the
badges on `main` will stay green; the card body matches what was built.

## Go standards

**Sources, in precedence order:** the OpenTelemetry Collector coding guidelines
(component shape), the Google Go Style Guide and its Best Practices, the Uber Go Style
Guide where Google is silent, and Go Code Review Comments and Effective Go as the
baseline everyone assumes. CI enforces what a tool can; review enforces the rest.

- **Layout.** `receiver/otlpsingleport`, `extension/oidcclientauth`, `internal/render`
  (environment → config), `internal/upstream` (the `OTEL_EXPORTER_OTLP_*` resolver),
  `internal/build` (version from ldflags), `integration/` (with the harness in
  `integration/harness`), `cmd/otlp-collector-oidc` (our `main` plus ocb's generated
  `components.go`), `internal/render/collector.yaml.tmpl` (the shipped pipeline,
rendered from the environment by `run`). One Go module.
  Package names are short, lower-case, and say what they provide; no `util`,
  `common`, `helpers`. A `doc.go` per package with the package comment.
- **Collector component conventions.** `factory.go` + `config.go` per component;
  `createDefaultConfig` returns every default and `Config.Validate()` fails fast on
  anything invalid; `metadata.yaml` with `mdatagen` generating `internal/metadata`
  (type, stability, and **every custom metric**, emitted through the generated
  `TelemetryBuilder`); `Config` suffix for YAML-facing structs, `Settings` for
  code-facing ones, no embedded config structs; enumerations are typed with the type
  name as the constant prefix; never `os.Exit` or `log.Fatal` outside `main`; never
  crash after startup and never on bad input (log, count, refuse); every queue and
  cache bounded; `Shutdown` stops accepting, cancels background work, honours its
  context.
- **Logging.** Product code logs only through the `*zap.Logger` handed in via
  `component.TelemetrySettings`; `fmt.Print*`, `log.*`, `println` and `os.Exit` are
  banned outside `main` and tests by `forbidigo`, so every line the process writes
  goes through the one path the own-logs work exports upstream.
- **Errors.** Wrap with `fmt.Errorf("<context>: %w", err)`; sentinels exported as
  `ErrXxx`, error types as `XxxError`; error strings lower-case, no trailing
  punctuation; handle an error once (log or return, never both); client faults become
  `consumererror.NewPermanent`; no `panic` outside programming errors at construction.
- **Interfaces and dependencies.** Define interfaces where they are consumed, keep
  them small, accept interfaces and return concrete types; assert compliance at
  compile time (`var _ extensionauth.Server = (*authenticator)(nil)`); inject clocks,
  HTTP clients and key sets; no package-level mutable state (the exceptions: the
  linker-set version in `internal/build`, and in test code the integration harness's
  set of ports it has handed out, which every parallel test must share); no `init()`.
- **Concurrency.** Every goroutine has an owner and an exit path (`context`,
  `errgroup`, or a server's `Stop`), never fire-and-forget; mutexes are value fields
  next to what they guard; `context.Context` is the first parameter wherever work can
  be cancelled.
- **Enumerations are exhaustive.** Any label value, reason, mode or protocol is a
  typed enum switched with the `exhaustive` linter on, so adding a variant fails the
  build until it is handled. This is how the bounded-label rule is enforced in code.
- **Formatting and linting, CI-gated.** `gofumpt` and `gci` (stdlib, third-party,
  module) as golangci-lint v2 formatters; `.golangci.yml` is `version: "2"`,
  `default: standard` plus the linters it lists; `go vet` and `govulncheck` in CI;
  every `nolint` directive names its linter and gives a reason. The Go version is
  pinned by the `go` and `toolchain` directives; Dependabot runs weekly for `gomod`
  and `github-actions`.
- **Versions move together.** The collector core, contrib, `ocb` and `mdatagen`
  versions in `builder.yaml` and `go.mod` are one release line, bumped in lockstep in
  a single PR, never individually (`make versions-check`; Dependabot ignores them).
  `components.go` and every `internal/metadata` are regenerated in that PR
  (`make generate`; CI's `generate-check` fails on drift).
- **Version identity.** There is no version file. The workspace version is the git
  tag (`v0.N.P`); `scripts/version.sh` derives it and the Makefile, Dockerfile and
  workflows link it into `internal/build`. A tagged commit reports `0.N.P`; any other
  commit reports the next minor, `0.(N+1).0-dev+<sha>` (before the first tag,
  `0.1.0-dev+<sha>`), and the `:edge` image carries that. Iteration tags are created
  by the release workflow on merge (see "Versioning"). `:latest` is published only
  from `v1.0.0` onwards; before that, only `:edge` and exact version tags exist.
- **Testing.** `testify` `require`/`assert`; table-driven with named struct fields;
  `t.Helper()` in helpers; `t.Parallel()` where the subprocess allows; no sleeps,
  `require.Eventually` for anything asynchronous; failures print expected, actual and
  the input. **No private key, certificate or token is ever committed**, test ones
  included: the harness generates ephemeral keys and certificates at run time, the
  image generates its own at build, and CI fails on any tracked PEM block.
- **Documentation.** Every exported name has a doc comment that starts with the name;
  comments say why, not what; configuration is documented from one source, never by
  hand twice.
- **Security.** Tokens, keys and headers never appear in logs, errors, spans or test
  output; `gosec` on; `crypto/rand` only.

## Status

Pre-release. Iteration 1 (`0.1.0`) delivers the module, the single-port receiver, the shipped
pipeline for traces and logs, the image, CI with its test report and badges, and the
integration harness. Iteration 2 (`0.2.0`) authenticates every request with an OIDC
access token (`extension/oidcclientauth`, `docs/token-profile.md`) and adds a fake
issuer to the harness. Iteration 3 (`0.3.0`) renders the configuration from the
environment (`internal/render`, `run`, `docs/configuration.md`). Iteration 4 (`0.4.0`)
resolves the upstream from the standard `OTEL_EXPORTER_OTLP_*` variables per signal
(`internal/upstream`), gRPC or HTTP, with headers, mTLS, queue and retry. Iteration 5
(`0.5.0`) sends the process's own logs to the logs upstream as OTLP
(`internal/render/selftelemetry.go`, `LOG_OUTPUT`, `OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES`). Iteration 6 (`0.6.0`) ships the dashboard
(`dashboards/`), the alert rules (`alerts/`) and `docs/runbook.md`; every card that
adds a metric adds its panel, and where warranted its alert and runbook entry, in the
same PR, and `TestArtefactMetrics` proves every name they query is exported.
Iteration 7 (`0.7.0`) stamps the token's identity (`CLAIM_ATTRIBUTES`, via
`attributes/identity`) and the deployment's attributes (`CLIENT_RESOURCE_ATTRIBUTES`,
via `resource/identity`, which also removes any identity a client put on the resource) onto every span and log record (`internal/render/identity.go`).
Iteration 8 (`0.8.0`) adds the authentik guide and blueprint (`docs/providers/`),
verified end to end against authentik 2026.8.3. Iteration 9 (`0.9.0`) adds the proxy
guide (`docs/proxies/traefik.md`), the compose example, and the behind-Traefik tier
(`TestBehindTraefik`: a real Traefik container, host network, in front of the
collector, running the fidelity and token-profile tables unchanged; it needs Docker,
which CI's runners and `make integration` have). Iteration 10 (`0.10.0`) bounds what a
client can cost (`internal/render/bounds.go`: `ALLOWED_SERVICE_NAMES`, now required,
`MAX_PAST_AGE`, `MAX_FUTURE_SKEW`), with a dashboard panel and runbook entry.
Iteration 11 (`0.11.0`) accepts client metrics on their own chain
(`internal/render/metrics.go`), with no identity action, allowlisted by name and key,
delta to cumulative under `MAX_METRIC_STREAMS`; the structural test
`TestMetricsPipelineCarriesNoIdentity` and the identifier-cardinality test in
`TestMetrics` guard the trust boundary. The MVP sign-off is next.

**Deviations from the `observability` skill §2, by design (DESIGN §3.4):** the
product's own metrics are a Prometheus pull on `:8888`, not an OTLP push — it is a
collector, and `:8888` is the collector's own contract; and it emits no traces of its
own — the request path is the stock receiver and processors, whose spans would
describe the collector's internals rather than anything an operator acts on. Its logs
are the one self-signal it pushes, as OTLP, to the logs upstream.
