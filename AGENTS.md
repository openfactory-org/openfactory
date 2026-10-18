# AGENTS.md

Instructions for AI coding agents and humans who change this repository. Read this file before you edit code. [CONTRIBUTING.md](CONTRIBUTING.md) has the same rules for people, with more context.

## Project

Open Factory runs teams of AI coding agents ("factories") against your repositories. One Go binary, `openfactory`, is the control plane (REST API, dashboard, engine, dispatcher), the remote worker, the in-sandbox runner and the CLI. Factories are YAML definition trees. Agents run in sandboxes (`local`, `kubernetes`, or a plugin backend) under a network policy. State lives in SQLite (development) or PostgreSQL. The dashboard is a React app in `web/`, built into `internal/webui/dist` and embedded in the binary. Go module: `github.com/openfactory-org/openfactory`. License: Apache-2.0.

## Repository map

| Path | Contents |
|---|---|
| `cmd/openfactory/` | The CLI and all subcommands (`serve`, `worker`, `runner`, `validate`, client commands). |
| `internal/core/` | Shared domain types, interfaces (`Store`, `Forge`, `Harness`, `SecretStore`, ...) and sentinel errors. |
| `internal/server/` | Wires every component into one HTTP server. The only place that connects concrete types. |
| `internal/api/` | REST API `/api/v1`, error codes (`errors.go`), the hand-maintained `openapi.json`. `apitest/` has fakes. |
| `internal/engine/` | Orchestration core: applies definitions, starts and advances runs. |
| `internal/runstate/` | Run state transitions. |
| `internal/dispatch/`, `internal/worker/` | Start sandboxes for runs; helpers for planner and remote workers. |
| `internal/runner*/` | The in-sandbox runner (`runner`), its client, protocol types, tokens, and the control-plane side (`runnerapi`). |
| `internal/sandbox/` | Sandbox backends: `tasklocal`, `kubernetes`, `k8stask`, `direct`, `egress`, `netguard`. |
| `internal/networkpolicy/`, `internal/egressdiag/` | Outbound network policies for runs; diagnostics for blocked egress. |
| `internal/definition/` | Parses, validates and compiles definitions. JSON Schemas in `schemas/v1/`. |
| `internal/store/` | `sqlite/`, `postgres/` (each with `migrations/`), shared `sqlstore/`, `storetest/` conformance suite, `testdb/`, `coord/`. |
| `internal/auth/`, `internal/runauth/`, `internal/mcpauth/` | User login and authorization; run bearer tokens; MCP browser sign-in. |
| `internal/secrets/`, `internal/keys/`, `internal/rewrap/`, `internal/redact/` | Envelope-encrypted secrets, key provider, key rotation, secret redaction in text. |
| `internal/harness/`, `internal/prompts/`, `internal/skills/` | Agent harness registry, system prompt composition, built-in skills. |
| `internal/forge/`, `internal/integrations/`, `internal/triggers/`, `internal/webhooks/`, `internal/notify/` | Code forges, work-source integrations, automation matching, webhook ingress, outbound messages. |
| `internal/mcpserver/`, `internal/orchestration/`, `internal/mcpregistry/` | Factory MCP server, per-run `factory` MCP tool server, team MCP registry. |
| `internal/plugin/` | Plugin host (discovery, gRPC). |
| `internal/config/` | Server configuration from defaults, file and environment. |
| `internal/events/`, `internal/eventctx/`, `internal/conversation/` | Event buses; event context of run messages; normalized run conversation. |
| `internal/objectstore/`, `internal/snapshot/` | Run data storage (transcripts, artifacts); git working directory snapshots. |
| `internal/metrics/`, `internal/scoring/`, `internal/spend/` | Factory dashboard metrics, scorers, model spend limits. |
| `internal/cloudid/`, `internal/mergeguard/`, `internal/links/` | Short-lived OIDC tokens for runs; the rule that agents open pull requests and people merge them; dashboard URLs. |
| `internal/naming/` | Repository checks for the product name and the glossary. |
| `internal/webui/` | Serves the embedded dashboard from `dist/`. |
| `pkg/sdk`, `pkg/openfactory`, `pkg/plugin/` | Public Go API (experimental): REST client, custom-binary layer, plugin SDK and generated protobuf code. |
| `proto/openfactory/plugin/{v1,v2}` | Plugin protocol. Generated into `pkg/plugin/pb` and `pkg/plugin/pb/v2`. |
| `plugins/` | Bundled plugins (`gitlab`, `azuredevops`), built by `make plugins`. |
| `web/` | React dashboard (Vite, TypeScript, vitest). |
| `deploy/` | Helm chart (`helm/openfactory`, tests in `helm/chart_test.go`), Dockerfiles, local test stack (`local/`, `aws-local/`). |
| `images/` | Tool images (`tool-claude`, `tool-codex`, `tool-example`). |
| `examples/` | Example definitions and two separate Go modules (`custom-binary`, `plugin-hello`). |
| `scripts/` | Generators and checks (`definition-docs`, `openapi-baseline`, `devseed`, `local/`, `images/`). |
| `docs/` | User and operator documentation. |
| `api/openapi.v1.baseline.json` | REST API baseline for the breaking-change report. |

## Build, run, test

Tools: Go (version in `go.mod`), `git`. For `make ci` also: `golangci-lint` v2, `govulncheck`, `shellcheck`, Helm 3, Node.js 22 with npm (CI uses 22; `web/package.json` allows 20+). Docker or Podman only for images and the local stack.

Fast loop while you work:

```bash
go build ./...
go test ./internal/<pkg>/... -count=1   # the package you changed
make fmt vet
make build                              # bin/openfactory
make run-dev                            # dev server: SQLite in ~/.openfactory, plugins from bin/plugins
make dev-seed                           # sample data for the dev server
```

Before you send a change, run the full gate. It mirrors `.github/workflows/ci.yml` (keep the two in step):

```bash
make ci        # stops at the first failure
make -k ci     # shows every failure
```

`make ci` runs: `fmt-check vet lint vuln shellcheck test-race build plugins examples-go validate-examples schemas-check docs-check helm-test openapi-check upgrade-test web-test web-check-api web-check`.

Other targets: `make help` lists all of them.

| Need | Command |
|---|---|
| Dashboard tests | `make web-test` (or `cd web && npm test`) |
| Dashboard dev server | `cd web && npm run dev` |
| Helm chart lint and render tests | `make helm-test` |
| Database upgrade tests | `make upgrade-test` |
| PostgreSQL store tests | Set `OF_TEST_POSTGRES_DSN`, then `go test -count=1 ./internal/store/...` |
| Live S3 tests | Set `OF_TEST_S3_URL`, then `go test -count=1 -run Live ./internal/objectstore` |
| Kubernetes end-to-end tests | `make e2e-k8s` with `OF_E2E_KUBE_CONTEXT` set |
| Full local stack (Kubernetes, PostgreSQL, AWS emulator) | `make local-up`, `make local-check`, `make local-down` |
| Images and smoke checks | `make images-check` |

Tests that need PostgreSQL, S3 or a cluster skip when their variable is not set. See [docs/self-hosting/local-testing.md](docs/self-hosting/local-testing.md) for the Docker commands and the local stack.

## Change rules: regenerate together

Generated files are committed. CI fails when they are stale. Never edit generated files by hand.

| If you change | Also do | Check |
|---|---|---|
| A REST route or its payload | Edit `internal/api/openapi.json` by hand. Run `cd web && npm run gen:api` to update `web/src/api/schema.d.ts`. Run `make web-build` and commit `internal/webui/dist`. | `TestOpenAPICoversRoutes`, `make web-check-api`, `make openapi-check` |
| Anything in `web/src/` | `make web-build`, then commit `internal/webui/dist`. | `make web-check` |
| A definition field (`internal/definition`) | Update validation, run `make schemas`, run `make docs-gen`, update `examples/` and the prose of `docs/definition-reference.md`. | `make schemas-check docs-check validate-examples` |
| A `.proto` file | `go generate ./pkg/plugin/pb` (needs `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`). | Build and plugin tests |
| The database schema | Add a migration to both `internal/store/sqlite/migrations` and `internal/store/postgres/migrations`. | `go test ./internal/store/...`, `make upgrade-test` |
| A CLI flag, env var or config key | Update `docs/` in the same change. | Review |
| The Helm chart | Add or update a case in `deploy/helm/chart_test.go`. | `make helm-test` |

Rules:

- `internal/webui/dist` comes only from `make web-build`. Do not edit it.
- `*.pb.go` files come only from `go generate`. Do not edit them.
- The field tables in `docs/definition-reference.md` come from `make docs-gen`. Do not change their structure by hand.
- Migrations: name files `<version>_<name>.sql` with a new, higher version. Never edit a released migration; add a new one. `storetest/upgrade.go` records the sha256 of released files. Keep the SQLite and PostgreSQL schemas equivalent.
- Proto: never renumber or reuse a field number. A breaking change needs a new package version and handshake version.
- `make openapi-baseline` runs only when a release is cut.

## Code conventions

- Read the code around your change and follow its style. `gofmt -s` and `.golangci.yml` are the baseline.
- Dependencies flow through `internal/core` interfaces. Packages get their dependencies injected; only `internal/server` wires concrete types. Do not import a sibling package to reach its internals.
- Errors: wrap the `internal/core` sentinels so the API maps them to error codes, for example `fmt.Errorf("%w: definition has %d validation error(s)", core.ErrInvalid, n)`. Use `ErrNotFound`, `ErrConflict`, `ErrInvalid`, `ErrUnauthorized`, `ErrForbidden`, `ErrUnavailable`. Compare with `errors.Is`. Wrap other errors with `%w` and context.
- Logs: `log/slog`. Pass `context.Context` as the first argument.
- IDs: `core.NewID("prefix")`.
- HTTP handlers expose `Register(mux *http.ServeMux)` and use method patterns such as `"POST /api/v1/factories/{uid}/runs"`.
- Go tests sit next to the code (`*_test.go`). Dashboard tests are in `web/src/test/`. Write table-driven tests. Use fakes for `internal/core` interfaces (see `internal/api/apitest`) and `httptest` for external APIs. Use `internal/store/testdb.New(t)` for a migrated in-memory SQLite store. A new `core.Store` method needs a case in `internal/store/storetest`, which runs against SQLite and PostgreSQL.
- Do not add a dependency (Go or npm) when the standard library or an existing dependency is enough. Explain each new one in the pull request.
- `pkg/` is a public, experimental Go API. CI reports its changes (`make go-apidiff`). Change it only on purpose.

## Docs and writing style

- Write plain technical English: short sentences, active voice, present tense, one term for one thing. Use the imperative for steps. Put a condition before its command.
- No marketing words, filler or emoji. Prefer tables and numbered steps.
- Every flag, env var, field and command you name must exist. Verify with the code or `go run ./cmd/openfactory <command> --help`.
- Keep relative links valid.
- `internal/naming/naming_test.go` checks `docs/`, `examples/`, `web/src/`, `deploy/helm/` and the CLI help text against the glossary ([docs/overview.md](docs/overview.md#glossary)). Read its `forbidden` list before you write user text. Run `go test ./internal/naming/ -count=1`.
- Use the machine name `openfactory`. The test rejects the old plural form.

## Security rules

- Never commit secrets, tokens, private keys, real credentials or `.db` files. Test values must be clearly fake.
- Never put personal names, personal emails or local machine paths in the repository.
- Do not weaken sandbox, network policy, authorization or redaction defaults. If a default must change, add tests that show the new behavior, and say so in the pull request.
- CLI help must not show secret values from the environment (`cmd/openfactory/help_test.go`).
- Report vulnerabilities as described in [SECURITY.md](SECURITY.md), not in public issues.

## Pull requests

- One topic per pull request. Open an issue first for a large change.
- Add or update tests for each behavior change. Update `docs/` when behavior, flags, env vars or definition fields change.
- `make ci` must pass.
- Commit subject: imperative, 72 characters or less, prefixed with the package or area, for example `engine: retry claim on conflict`. The body tells what changed and why. Link the issue.
- Fill in `.github/pull_request_template.md`: what changed, how you tested it, what remains.
