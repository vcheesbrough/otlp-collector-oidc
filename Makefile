GO            ?= go
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK   ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@v1.8.0
GOTESTSUM     ?= $(GO) run gotest.tools/gotestsum@v1.13.0
IMAGE         ?= ghcr.io/vcheesbrough/otlp-collector-oidc
VERSION       ?= $(shell scripts/version.sh)
REVISION      ?= $(shell git rev-parse HEAD)
CREATED       ?= $(shell git log -1 --format=%cI HEAD)

MODULE  := github.com/vcheesbrough/otlp-collector-oidc
LDFLAGS := -s -w -X $(MODULE)/internal/build.version=$(VERSION)
BIN     := bin/otlp-collector-oidc
# Unit packages: everything but the integration tier.
UNIT    := $(shell $(GO) list ./... | grep -v /integration)

.PHONY: all fmt lint vet vuln test integration build image generate generate-check versions-check docs docs-check release-plan-test alerts-check check

all: check build

## fmt: rewrite sources with gofumpt and gci.
fmt:
	$(GOLANGCI_LINT) fmt

## lint: fail on any formatting diff or linter finding.
lint:
	$(GOLANGCI_LINT) fmt --diff
	$(GOLANGCI_LINT) run

vet:
	$(GO) vet ./...

vuln:
	$(GOVULNCHECK) ./...

## test: unit tests only; the integration tier is `make integration`.
test:
	$(GO) test $(UNIT)

## integration: build the collector with coverage and drive it from outside.
integration:
	$(GO) test -count=1 -timeout 5m ./integration

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/otlp-collector-oidc

image:
	docker build --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) --build-arg CREATED=$(CREATED) -t $(IMAGE):dev .

## generate: mdatagen for every component, ocb for the distribution's components.go.
generate:
	cd receiver/otlpsingleport && $(GO) tool mdatagen metadata.yaml
	cd extension/oidcclientauth && $(GO) tool mdatagen metadata.yaml
	$(GO) tool builder --config builder.yaml --skip-compilation --skip-get-modules
	cp _build/components.go cmd/otlp-collector-oidc/components.go

## generate-check: fail if generated code differs from what is committed.
generate-check: generate
	@git diff --exit-code -- receiver extension cmd || (echo "generated code is stale: run make generate" >&2; exit 1)
	@test -z "$$(git status --porcelain -- receiver extension cmd)" || (git status --porcelain -- receiver extension cmd >&2; echo "generated files are not committed" >&2; exit 1)

## versions-check: builder.yaml and go.mod pin the same collector release.
versions-check:
	scripts/versions-check.sh

## docs: regenerate docs/configuration.md and the renderer's golden files from internal/render.
docs:
	$(GO) test -count=1 ./internal/render -run 'TestReferenceIsCurrent|TestGolden' -update

## docs-check: the template renders every variable and nothing else, and docs/configuration.md is current.
docs-check:
	$(GO) test -count=1 ./internal/render -run 'TestTemplateUsesEverySetting|TestReferenceIsCurrent|TestReferenceListsEveryVariable'

## release-plan-test: the rules that decide what a release publishes.
release-plan-test:
	scripts/release-plan_test.sh

## alerts-check: the alert rules parse and pass their unit tests (promtool).
PROMTOOL_IMAGE ?= prom/prometheus:v3.13.1
alerts-check:
	docker run --rm -v "$(CURDIR)/alerts:/alerts:ro" --entrypoint promtool $(PROMTOOL_IMAGE) check rules /alerts/otlp-collector-oidc.yaml
	docker run --rm -v "$(CURDIR)/alerts:/alerts:ro" -w /alerts/tests --entrypoint promtool $(PROMTOOL_IMAGE) test rules otlp-collector-oidc_test.yaml

check: lint vet test generate-check versions-check docs-check release-plan-test
