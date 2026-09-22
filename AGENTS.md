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
| **CI** | **GitHub Actions**, not Woodpecker — baseline §4's GitHub-native path applies: poll `gh api repos/vcheesbrough/otlp-collector-oidc/commits/$SHA/status --jq '.state'` |
| **Image** | `ghcr.io/vcheesbrough/otlp-collector-oidc` (multi-arch, published by `release.yml` on `v*` tags; `main` → `:edge`) |
| **Language** | Go — a custom OpenTelemetry Collector distribution built with `ocb` |

Shared skills apply here once the machine is wired up (`start-iteration`,
`ci-watch`, `pr-review-loop`). Their per-repo parameters:

- `OWNER=vcheesbrough`, `REPO=otlp-collector-oidc`
- CI reproduce commands: `go test ./...`, `ocb --config builder.yaml`, and the
  integration suite `go test ./integration` (once they exist — see status below)
- Extra review criteria beyond the baseline five (correctness, security/OWASP, tests,
  versioning, scope): every change to `extension/` or `receiver/` is on the trust
  boundary — the reviewer checks that no client-supplied value can reach a metric
  label, that every refusal names its reason and is counted, and that nothing in the
  metrics pipeline references an `auth.` identity key.

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

### Status

Design stage. The repository holds the licence, the design and its decision record;
no code, build or CI exists yet. The first iteration scaffolds the module and proves
the single-port receiver (`docs/DESIGN.md` §10, first item) before anything depends
on it.
