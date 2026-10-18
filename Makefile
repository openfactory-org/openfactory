# Open Factory - developer tasks. Run `make help` for the list.

BINARY      ?= openfactory
CMD         := ./cmd/openfactory
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# DEFAULT_SANDBOX_IMAGE sets the default sandbox image of the binary (for example
# ghcr.io/openfactory-org/sandbox:1.2.3). Empty keeps the default of the source.
DEFAULT_SANDBOX_IMAGE ?=
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE) \
  $(if $(DEFAULT_SANDBOX_IMAGE),-X github.com/openfactory-org/openfactory/internal/sandbox.DefaultImage=$(DEFAULT_SANDBOX_IMAGE))
GOFLAGS     ?= -trimpath
GOFILES      = $(shell find . -name '*.go' -not -path './vendor/*' -not -path './dist/*')

IMAGE         ?= ghcr.io/openfactory-org/openfactory
SANDBOX_IMAGE ?= ghcr.io/openfactory-org/sandbox
TOOL_CLAUDE_IMAGE ?= ghcr.io/openfactory-org/tool-claude
TOOL_CODEX_IMAGE  ?= ghcr.io/openfactory-org/tool-codex
# Harness CLI packages and versions of the tool images.
CLAUDE_CLI_PACKAGE ?= @anthropic-ai/claude-code
CLAUDE_CLI_VERSION ?= latest
CODEX_CLI_PACKAGE  ?= @openai/codex
CODEX_CLI_VERSION  ?= latest
NODE_VERSION       ?= 22
IMAGE_TAG     ?= $(VERSION)
PLATFORMS     ?= linux/amd64,linux/arm64

GOLANGCI_LINT ?= golangci-lint
# govulncheck: the one on PATH, else the one that `go install` put in GOPATH/bin.
GOVULNCHECK   ?= $(or $(shell command -v govulncheck 2>/dev/null),$(shell go env GOPATH)/bin/govulncheck)
# Pinned contract-check tools (run through `go run`, nothing to install).
OASDIFF       ?= github.com/oasdiff/oasdiff@v1.34.0
APIDIFF_BASE  ?= origin/main
OPENAPI_SPEC  := internal/api/openapi.json
OPENAPI_BASE  := api/openapi.v1.baseline.json

.DEFAULT_GOAL := build

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

PLUGINS     := gitlab azuredevops

.PHONY: build
build: ## Build bin/openfactory.
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

.PHONY: plugins
plugins: ## Build the bundled gRPC plugins into bin/plugins.
	@set -e; for p in $(PLUGINS); do \
	  echo "build bin/plugins/openfactory-plugin-$$p"; \
	  CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "-s -w" -o bin/plugins/openfactory-plugin-$$p ./plugins/$$p; \
	done

.PHONY: build-all
build-all: build plugins ## Build the binary and the bundled plugins.

.PHONY: example-custom-binary
example-custom-binary: ## Build examples/custom-binary into bin/custom-openfactory.
	cd examples/custom-binary && go build -o ../../bin/custom-openfactory .

.PHONY: example-plugin-hello
example-plugin-hello: ## Build examples/plugin-hello into bin/example-plugins.
	cd examples/plugin-hello && go build -o ../../bin/example-plugins/openfactory-plugin-hello .

.PHONY: examples-go
examples-go: example-custom-binary example-plugin-hello ## Build every Go example module.

.PHONY: install
install: ## Install openfactory into GOBIN.
	CGO_ENABLED=0 go install $(GOFLAGS) -ldflags "$(LDFLAGS)" $(CMD)

.PHONY: test
test: ## Run unit tests.
	go test ./...

.PHONY: test-race
test-race: ## Run unit tests with the race detector.
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests with a coverage report (coverage.out).
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

.PHONY: validate-examples
validate-examples: build ## Validate every example definition with the binary.
	@set -e; for d in examples/*/; do [ -f "$$d/factory.yaml" ] || continue; echo "validate $$d"; ./bin/$(BINARY) validate "$$d"; done

.PHONY: schemas-check
schemas-check: ## Fail when the definition JSON Schemas are stale.
	go test ./internal/definition -run TestSchemasUpToDate -count=1

.PHONY: schemas
schemas: ## Regenerate the definition JSON Schemas (internal/definition/schemas).
	UPDATE_SCHEMAS=1 go test ./internal/definition -run TestSchemasUpToDate -count=1

.PHONY: docs-gen
docs-gen: ## Regenerate the field tables of docs/definition-reference.md.
	go run ./scripts/definition-docs

.PHONY: docs-check
docs-check: ## Fail when the field tables of docs/definition-reference.md are stale.
	go run ./scripts/definition-docs -check

.PHONY: run-dev
run-dev: build plugins ## Run the server in dev mode (SQLite, generated keys).
	OF_DEV=true OF_LOG_LEVEL=debug OF_PLUGIN_DIR=bin/plugins ./bin/$(BINARY) serve

.PHONY: dev-seed
# dev-seed writes to OF_DATA_DIR (default ~/.openfactory).
dev-seed: ## Fill a dev database with sample runs, work items and inbox items.
	go run ./scripts/devseed -db "$${OF_DATA_DIR:-$$HOME/.openfactory}/openfactory.db"

.PHONY: docker
docker: ## Build the server image for the local platform.
	docker build -f deploy/docker/Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(IMAGE_TAG) -t $(IMAGE):latest .

.PHONY: docker-push
docker-push: ## Build and push the multi-arch server image.
	docker buildx build --platform $(PLATFORMS) -f deploy/docker/Dockerfile \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(IMAGE_TAG) --push .

.PHONY: sandbox-image
sandbox-image: ## Build the default sandbox image.
	docker build -f deploy/sandbox/Dockerfile \
	  -t $(SANDBOX_IMAGE):$(IMAGE_TAG) -t $(SANDBOX_IMAGE):latest deploy/sandbox

.PHONY: image-tool-claude
image-tool-claude: ## Build the tool image with the claude CLI (images/tool-claude).
	docker build -f images/tool-claude/Dockerfile --build-arg NODE_VERSION=$(NODE_VERSION) \
	  --build-arg CLAUDE_CLI_PACKAGE=$(CLAUDE_CLI_PACKAGE) --build-arg CLAUDE_CLI_VERSION=$(CLAUDE_CLI_VERSION) \
	  -t $(TOOL_CLAUDE_IMAGE):$(IMAGE_TAG) -t $(TOOL_CLAUDE_IMAGE):latest images/tool-claude

.PHONY: image-tool-codex
image-tool-codex: ## Build the tool image with the codex CLI (images/tool-codex).
	docker build -f images/tool-codex/Dockerfile --build-arg NODE_VERSION=$(NODE_VERSION) \
	  --build-arg CODEX_CLI_PACKAGE=$(CODEX_CLI_PACKAGE) --build-arg CODEX_CLI_VERSION=$(CODEX_CLI_VERSION) \
	  -t $(TOOL_CODEX_IMAGE):$(IMAGE_TAG) -t $(TOOL_CODEX_IMAGE):latest images/tool-codex

# Images and end-to-end checks that CI does not run. They need Docker or Podman.
# See docs/self-hosting/local-testing.md#real-world-checks.
# IMAGES limits the images, for example: make images IMAGES="server tool-claude".
# The default is every image except the GoReleaser one (add "release" for it).
IMAGES ?=
IMAGES_ENV = IMAGE_TAG=$(IMAGE_TAG) VERSION=$(VERSION) COMMIT=$(COMMIT) DATE=$(DATE) IMAGE=$(IMAGE) \
  SANDBOX_IMAGE=$(SANDBOX_IMAGE) TOOL_CLAUDE_IMAGE=$(TOOL_CLAUDE_IMAGE) \
  TOOL_CODEX_IMAGE=$(TOOL_CODEX_IMAGE) NODE_VERSION=$(NODE_VERSION) \
  CLAUDE_CLI_PACKAGE=$(CLAUDE_CLI_PACKAGE) CLAUDE_CLI_VERSION=$(CLAUDE_CLI_VERSION) \
  CODEX_CLI_PACKAGE=$(CODEX_CLI_PACKAGE) CODEX_CLI_VERSION=$(CODEX_CLI_VERSION)

.PHONY: images
# images builds server, sandbox, tool-claude, tool-codex and tool-example with docker or podman.
images: ## Build every image.
	$(IMAGES_ENV) sh scripts/images/build.sh build $(IMAGES)

.PHONY: images-smoke
images-smoke: ## Run the smoke checks on the images that `make images` built.
	$(IMAGES_ENV) sh scripts/images/build.sh smoke $(IMAGES)

.PHONY: images-check
images-check: ## Build every image, then run the smoke checks.
	$(IMAGES_ENV) sh scripts/images/build.sh all $(IMAGES)

.PHONY: e2e-k8s
# e2e-k8s tests the deadline, a Role without jobs and network limits.
# Set OF_E2E_RUNNER_IMAGE (deadline) and OF_E2E_PROXY_IMAGE (network).
e2e-k8s: ## Run the Kubernetes end-to-end tests on OF_E2E_KUBE_CONTEXT.
	@test -n "$(OF_E2E_KUBE_CONTEXT)" || { echo "set OF_E2E_KUBE_CONTEXT to a kube context that you may use"; exit 1; }
	OF_E2E_KUBE_CONTEXT="$(OF_E2E_KUBE_CONTEXT)" go test -tags e2e_k8s -run '^TestK8s' -v -count=1 -timeout 15m ./internal/sandbox/k8stask/

.PHONY: aws-local-up
# aws-local-up runs the AWS emulator behind a TLS front in KUBE_CONTEXT (default orbstack).
aws-local-up: ## Start the local AWS stack (test only).
	sh deploy/aws-local/up.sh

.PHONY: aws-local-down
aws-local-down: ## Remove the local AWS stack and all its state.
	sh deploy/aws-local/down.sh

.PHONY: local-up
# local-up starts the AWS emulator, PostgreSQL, a git server, the claude CLI stand-in
# and 2 servers. Settings: LOCAL_SERVER_MODE, LOCAL_MODEL, LOCAL_FORGE, LOCAL_BUILD,
# KUBE_CONTEXT, LOCAL_DOMAIN, LOCAL_DIR. KUBE_CONTEXT=kind-<name> creates a kind cluster.
local-up: ## Start the local test stack.
	sh deploy/local/up.sh

.PHONY: local-down
local-down: ## Stop the local test stack and delete its state (keeps LOCAL_DIR).
	sh deploy/local/down.sh

.PHONY: local-status
local-status: ## Show the health of the local test stack.
	@sh deploy/local/status.sh

.PHONY: local-env
local-env: ## Print export lines for the local test stack.
	@sh deploy/local/env.sh

.PHONY: local-check
# LOCAL_CHECKS=R1,R2 runs only some checks.
local-check: ## Run the checks against the local test stack.
	sh scripts/local/check.sh

.PHONY: test-aws-local
test-aws-local: ## Run the live S3 tests against the local AWS stack.
	@. "$${AWS_LOCAL_DIR:-$$HOME/.aws/emulator/aws-local}/server.env" && \
	  OF_TEST_S3_URL="$$OF_OBJECT_STORE_URL" OF_TEST_S3_MAX_OBJECTS=1000 \
	  go test -count=1 -run Live -v ./internal/objectstore

.PHONY: helm-test
helm-test: ## Lint the Helm chart and render it in every mode (needs helm).
	@command -v helm >/dev/null || { echo "helm is not installed: see https://helm.sh/docs/intro/install/"; exit 1; }
	helm lint deploy/helm/openfactory
	go test -count=1 ./deploy/helm/...

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
	go run ./scripts/openapi-baseline -check -out $(OPENAPI_BASE)
	go run $(OASDIFF) breaking $(OPENAPI_BASE) $(OPENAPI_SPEC) $(if $(filter 1,$(OPENAPI_ENFORCE)),--fail-on ERR)

.PHONY: go-apidiff
go-apidiff: ## Report Go API changes in pkg/ against APIDIFF_BASE (report only).
	sh scripts/go-apidiff.sh $(APIDIFF_BASE)

.PHONY: web
web: web-build ## Alias of web-build.

.PHONY: web-build
web-build: ## Build the React dashboard into internal/webui/dist (needs Node 20+).
	cd web && npm ci && npm run build

.PHONY: web-test
web-test: ## Run the dashboard unit tests (vitest).
	cd web && npm ci && npm test

.PHONY: web-check-api
web-check-api: ## Fail when web/src/api/schema.d.ts is stale against the OpenAPI spec.
	cd web && npm run check:api

.PHONY: web-check
web-check: web-build ## Fail when the committed dashboard build (internal/webui/dist) is stale.
	@if [ -n "$$(git status --porcelain -- internal/webui/dist)" ]; then \
	  git status --short -- internal/webui/dist; \
	  echo "internal/webui/dist is out of date: run 'make web-build' and commit the result"; exit 1; fi

.PHONY: upgrade-test
upgrade-test: ## Run the database upgrade tests.
	go test -count=1 ./internal/store/... -run Upgrade

.PHONY: release
release: ## Publish release archives and images with GoReleaser (needs a tag).
	goreleaser release --clean

.PHONY: snapshot
snapshot: ## Build a local GoReleaser snapshot (no publish).
	goreleaser release --snapshot --clean

.PHONY: clean
clean: ## Remove build output.
	rm -f bin/$(BINARY) bin/custom-openfactory coverage.out
	rm -rf bin/plugins bin/example-plugins
	rm -rf ./dist

# ci runs each check of .github/workflows/ci.yml, in order. Keep the two in step.
# It does not run the report-only Go API diff or the image builds (they need Docker).
.PHONY: ci
CI_CHECKS := fmt-check vet lint vuln shellcheck test-race build plugins examples-go validate-examples \
  schemas-check docs-check helm-test openapi-check upgrade-test web-test web-check-api web-check
# make -k ci shows all failures. The tools are listed in CONTRIBUTING.md.
ci: $(CI_CHECKS) ## Run every CI check locally.
