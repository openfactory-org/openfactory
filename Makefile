# Open Factory - developer tasks. Run `make help` for the list.
#
# The control plane, CLI, dashboard and providers are TypeScript (npm workspaces).
# The Go module builds only the runtime binary, openfactory-runtime.

RUNTIME     ?= openfactory-runtime
CMD         := ./cmd/openfactory
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
GOFLAGS     ?= -trimpath
GOFILES      = $(shell find . -name '*.go' -not -path '*/node_modules/*' \
  -not -path './vendor/*' -not -path './dist/*' -not -path './.claude/*' -not -path './.owner/*' -not -path './workspace/*')

SANDBOX_IMAGE ?= ghcr.io/openfactory-org/sandbox
IMAGE_TAG     ?= $(VERSION)
LINUX_ARCHES  ?= amd64 arm64

GOLANGCI_LINT ?= golangci-lint
# govulncheck: the one on PATH, else the one that `go install` put in GOPATH/bin.
GOVULNCHECK   ?= $(or $(shell command -v govulncheck 2>/dev/null),$(shell go env GOPATH)/bin/govulncheck)
# Pinned contract-check tool (run through `go run`, nothing to install).
OASDIFF       ?= github.com/oasdiff/oasdiff@v1.34.0
OPENAPI_SPEC  := api/openapi.json
OPENAPI_BASE  := api/openapi.v1.baseline.json

.DEFAULT_GOAL := build

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ---- Go runtime ----

.PHONY: build
build: ## Build bin/openfactory-runtime.
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(RUNTIME) $(CMD)

.PHONY: build-linux
build-linux: ## Build bin/linux-<arch>/openfactory-runtime for each of LINUX_ARCHES.
	@set -e; for a in $(LINUX_ARCHES); do \
	  echo "build bin/linux-$$a/$(RUNTIME)"; \
	  GOOS=linux GOARCH=$$a CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/linux-$$a/$(RUNTIME) $(CMD); \
	done

.PHONY: test
test: ## Run the Go unit tests.
	go test ./...

.PHONY: test-race
test-race: ## Run the Go unit tests with the race detector.
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run the Go tests with a coverage report (coverage.out).
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: fmt
fmt: ## Format Go code (gofmt -s).
	gofmt -s -w $(GOFILES)

.PHONY: fmt-check
fmt-check: ## Fail when Go code is not formatted.
	@out="$$(gofmt -s -l $(GOFILES))"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: lint
lint: ## Run golangci-lint (configuration in .golangci.yml).
	@command -v $(GOLANGCI_LINT) >/dev/null || { echo "golangci-lint is not installed: see https://golangci-lint.run/welcome/install/"; exit 1; }
	$(GOLANGCI_LINT) run ./...

.PHONY: vuln
vuln: ## Report known vulnerabilities (govulncheck).
	@command -v $(GOVULNCHECK) >/dev/null || { echo "govulncheck is not installed: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	$(GOVULNCHECK) ./...

.PHONY: naming
naming: ## Check the product name and the glossary.
	go test ./internal/naming -count=1

.PHONY: go-allowlist
go-allowlist: ## Fail when `go list ./...` differs from tools/go-allowlist.txt.
	@go list ./... | sort | diff -u tools/go-allowlist.txt - || { echo "go package list differs from tools/go-allowlist.txt"; exit 1; }

.PHONY: go-allowlist-update
go-allowlist-update: ## Rewrite tools/go-allowlist.txt from `go list ./...`.
	go list ./... | sort > tools/go-allowlist.txt

# ---- TypeScript workspace ----

.PHONY: node-ci
node-ci: ## Install with npm ci and run every TypeScript check (npm run ci).
	npm ci && npm run ci

.PHONY: gen
gen: ## Regenerate every generated file.
	npm run gen

.PHONY: gen-check
gen-check: ## Fail when a generated file is stale.
	npm run gen:check

.PHONY: schemas
schemas: ## Regenerate the definition JSON Schemas.
	npm run gen:schemas

.PHONY: docs-gen
docs-gen: ## Regenerate the field tables of docs/definition-reference.md.
	npm run gen:docs

.PHONY: validate-examples
validate-examples: ## Validate every example definition with the CLI.
	npm run validate-examples

.PHONY: contract
contract: ## Run the contract suite against the Worker (wrangler dev).
	npm run contract

# ---- Development ----

.PHONY: dev
dev: ## Run the control plane locally (wrangler dev).
	npm run dev

.PHONY: dev-worker
dev-worker: build ## Run a dev process worker against the local control plane.
	OF_SERVER=http://localhost:8787 ./bin/$(RUNTIME) worker --backend local --id dev

.PHONY: dev-seed
dev-seed: ## Fill the local control plane with sample data.
	npm run seed -w @openfactory/control-plane

.PHONY: web-dev
web-dev: ## Run the dashboard dev server.
	npm run dev -w @openfactory/web

# ---- Sandbox images, runner and kernel ----

.PHONY: sandbox-image
sandbox-image: ## Build the default sandbox image.
	docker build -f deploy/sandbox/Dockerfile \
	  -t $(SANDBOX_IMAGE):$(IMAGE_TAG) -t $(SANDBOX_IMAGE):latest deploy/sandbox

.PHONY: sandbox-image-cloudflare
sandbox-image-cloudflare: ## Build the Cloudflare Sandbox image.
	docker build -f deploy/sandbox/Dockerfile.cloudflare \
	  -t $(SANDBOX_IMAGE)-cloudflare:$(IMAGE_TAG) -t $(SANDBOX_IMAGE)-cloudflare:latest deploy/sandbox

.PHONY: images-check
images-check: ## Build the sandbox images, then run the smoke checks.
	IMAGE_TAG=$(IMAGE_TAG) SANDBOX_IMAGE=$(SANDBOX_IMAGE) sh scripts/images/build.sh all

.PHONY: boat-snapshot
boat-snapshot: ## Build the boat.dev snapshot of the sandbox image.
	sh scripts/boat-snapshot.sh

.PHONY: runner-publish
runner-publish: ## Build the runtime binaries and upload them with checksums.txt to R2.
	sh scripts/runner-publish.sh

.PHONY: kernel
kernel: ## Build the Firecracker guest kernel.
	sh images/firecracker-kernel/build.sh

# ---- Live checks (not part of ci) ----

.PHONY: e2e-providers
# Needs OF_TEST_DAYTONA_KEY, OF_TEST_BOAT_KEY and the Cloudflare account variables.
e2e-providers: ## Run the live provider tests (skip without credentials).
	npm run e2e -w tests/e2e

.PHONY: e2e-firecracker
e2e-firecracker: ## Run the microVM tests on a Linux host with KVM.
	OF_E2E_FIRECRACKER=1 go test -tags e2e_firecracker -count=1 ./internal/machine/...

# ---- Contracts and release ----

.PHONY: shellcheck
shellcheck: ## Lint the tracked shell scripts.
	{ git ls-files '*.sh'; git ls-files -z | xargs -0 awk 'FNR == 1 { if (/^#!.*sh/) print FILENAME; nextfile }'; } | \
		sort -u | xargs shellcheck

.PHONY: openapi-baseline
openapi-baseline: ## Regenerate the OpenAPI baseline (at release time).
	go run ./scripts/openapi-baseline -spec $(OPENAPI_SPEC) -out $(OPENAPI_BASE)

# Before the tag v1.0.0 the REST API can change, so the diff only reports.
# After v1.0.0, set OPENAPI_ENFORCE to 1 (here, and in .github/workflows/ci.yml).
OPENAPI_ENFORCE ?= 0

.PHONY: openapi-check
# openapi-check fails only with OPENAPI_ENFORCE=1.
openapi-check: ## Report breaking REST API changes (oasdiff).
	go run ./scripts/openapi-baseline -check -spec $(OPENAPI_SPEC) -out $(OPENAPI_BASE)
	go run $(OASDIFF) breaking $(OPENAPI_BASE) $(OPENAPI_SPEC) $(if $(filter 1,$(OPENAPI_ENFORCE)),--fail-on ERR)

.PHONY: release
release: ## Publish the runtime archives with GoReleaser (needs a tag).
	goreleaser release --clean

.PHONY: snapshot
snapshot: ## Build a local GoReleaser snapshot (no publish).
	goreleaser release --snapshot --clean

.PHONY: clean
clean: ## Remove build output.
	rm -rf bin dist web/dist apps/*/dist packages/*/dist .wrangler
	rm -f coverage.out

# ci runs each check of .github/workflows/ci.yml, in order. Keep the two in step.
# It does not run the image builds, the npm audit report or the Firecracker tests.
# make -k ci shows all failures. The tools are listed in CONTRIBUTING.md.
.PHONY: go-ci
go-ci: fmt-check vet lint vuln test-race build naming go-allowlist ## Run the Go checks.

.PHONY: ci
ci: node-ci go-ci shellcheck openapi-check ## Run every CI check locally.
