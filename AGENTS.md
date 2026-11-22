# AGENTS.md

Instructions for AI coding agents and humans who change this repository. Read this file before you edit code. [CONTRIBUTING.md](CONTRIBUTING.md) has the same rules for people, with more context.

## Project

Open Factory runs teams of AI coding agents ("factories") against your repositories. The control plane is a TypeScript Cloudflare Worker with Durable Objects (DO): REST API, dashboard (Static Assets), engine, dispatch, integrations and the Factory MCP. State lives in DO SQLite, run data in R2. Agent runs execute on hosted sandbox providers (`cloudflare-sandbox`, `daytona`, `boatdev`) or on self-hosted Linux workers that run Firecracker microVMs. One Go binary, `openfactory-runtime`, runs inside sandboxes and on worker hosts: the runner, the egress proxy (`netguard`), the microVM worker agent, `vm-init` and the dev process worker. The `openfactory` CLI is TypeScript, published to npm. Factories are YAML definition trees. Plugins are separate Workers called over service bindings. Go module: `github.com/openfactory-org/openfactory`. npm workspaces at the root. License: Apache-2.0.

## Repository map

| Path | Contents |
|---|---|
| `apps/control-plane/` | The control plane Worker (`@openfactory/control-plane`): `wrangler.jsonc`, `src/index.ts` (fetch, scheduled, queue; exports the DO classes), `src/builder.ts` (extension points), `src/contracts/` (`env.ts`, `do-messages.ts`), `src/http/` (router, `routes.ts`), `src/api/` (REST handlers), `src/do/<name>/` (DO classes and `migrations/`), `src/lib/` (DO base, outbox, timers, SQL), `src/engine/`, `src/runner-api/`, `src/auth/`, `src/secrets/`, `src/storage/`, `src/forges/`, `src/integrations/`, `src/mcp/`, `src/plugins/`, `src/scoring/`. Tests in `test/` and next to the code. |
| `apps/cli/` | The `openfactory` CLI: client commands, `validate`, `schema`, `init`, `mcp` (stdio bridge), forwarding of runtime commands to `openfactory-runtime`. Bundled to `dist/` by esbuild. |
| `packages/core/` | Domain types, IDs (`newId`, `newRoutedId`), errors (`OfError`), run states, network policy, redaction, prompts, built-in skills. |
| `packages/definition/` | Parses, validates and compiles definitions; schema and docs generators; goldens from the Go reference. |
| `packages/api-client/` | `schema.d.ts` generated from `api/openapi.json` and the typed REST client. |
| `packages/runner-proto/`, `packages/worker-proto/` | Types and validators generated from `schemas/runner/v1` and `schemas/worker/v2`. |
| `packages/sandbox/` | `SandboxProvider` interface, capability gate, failure mapping, `local` and microVM providers, conformance suite. |
| `packages/plugin-api/` | Plugin RPC interfaces (`WorkerEntrypoint`), `PLUGIN_API_VERSION`, test harness. |
| `providers/` | Sandbox providers: `cloudflare-sandbox` (its own Worker), `daytona`, `boatdev`. |
| `plugins/` | Plugin Workers: `gitlab`, `azuredevops`. |
| `web/` | React dashboard (Vite, TypeScript, vitest). Built to `web/dist`, served as Static Assets. |
| `tests/contract/` | Black-box contract suite: REST (driven by `api/openapi.json`), fake runner, fake worker, SSE. |
| `tests/e2e/` | Opt-in live scenarios (providers, cloud identity). They skip without credentials. |
| `api/` | `openapi.json` (the REST contract, edited by hand) and `openapi.v1.baseline.json` (breaking-change baseline). |
| `schemas/` | `definition/v1` (definition JSON Schemas, generated), `runner/v1` and `worker/v2` (protocol frames), `testdata/` (vectors shared by Go and TypeScript). |
| `tools/` | `gen/` (generators and the generated-file check), `contract/run.mjs`, `test-ledger.json` and its check, `go-allowlist.txt`, ownership check. |
| `cmd/openfactory/` | The `openfactory-runtime` binary: `runner`, `netguard`, `worker` (`enroll`, `run`, `status`, `drain`, `doctor`, or the dev process worker with `--process`), `vm-init`, `version`. |
| `internal/rtcore/` | Core types of the runtime (harness, task, sandbox reasons, failure codes). |
| `internal/runner/`, `internal/runnerclient/`, `internal/runnerproto/` | The in-sandbox runner, its `/runner/v1` client and the protocol structs. |
| `internal/harness/` | Harness adapters (`claude`, `codex`) and the harness test kit. |
| `internal/sandbox/` | `netguard` (egress proxy), `netlock` (in-sandbox lock), `tasklocal` (host processes for the dev worker), `direct` (worker protocol v2 client). |
| `internal/machine/` | The microVM worker: agent, VM manager, image pulls, git cache, snapshots, networking, `vm-init` guest, host checks, `worker.yaml` config. |
| `internal/snapshot/`, `internal/redact/`, `internal/egressdiag/`, `internal/worker/`, `internal/gitcachekey/` | Workspace snapshots, secret redaction, blocked-egress diagnostics, worker metrics, git cache keys. |
| `internal/testkit/`, `internal/naming/` | Test helpers (`fakeclaude`, `schematest`); the product name and glossary test. |
| `deploy/sandbox/` | The sandbox image (`Dockerfile`, `Dockerfile.cloudflare`, `install.sh`, `bootstrap.sh`). |
| `deploy/machine/` | Worker host install script and the `openfactory-worker` systemd unit. |
| `images/firecracker-kernel/` | The microVM guest kernel build. |
| `scripts/` | Image build and smoke checks, `boat-snapshot.sh`, `runner-publish.sh`, `openapi-baseline/`. |
| `examples/` | Example definitions, `plugin-worker/`, `control-plane-builder/`. |
| `docs/` | User and operator documentation. |

## Build, run, test

Tools: Node.js 24 with npm, Go (version in `go.mod`), `git`. For `make ci` also: `golangci-lint` v2, `govulncheck`, `shellcheck`. Docker or Podman only for the sandbox images. The control plane runs locally with `wrangler` (a root dev dependency); no Cloudflare account is needed for `npm run dev`.

Fast loop while you work:

```bash
npm ci                                   # install the workspace (once, and after a lockfile change)
npx vitest run --project <name>          # tests of one package (the directory name, for example control-plane)
npm test -w <package>                    # the same through the package script
npm run typecheck                        # tsc -b over the workspace
go test ./internal/<pkg>/... -count=1    # the Go package you changed
make build                               # bin/openfactory-runtime
npm run dev                              # control plane on http://localhost:8787 (wrangler dev)
make dev-worker                          # dev process worker against the local control plane
make dev-seed                            # sample data for the local control plane
npm run contract                         # contract suite against wrangler dev
```

`make dev-worker` needs `OF_DEV_PROCESS_WORKERS=true` in the control plane (`.dev.vars`).

Before you send a change, run the full gate. It mirrors `.github/workflows/ci.yml` (keep the two in step):

```bash
make ci        # node-ci go-ci shellcheck openapi-check; stops at the first failure
make -k ci     # shows every failure
npm run ci     # only the TypeScript checks
```

`npm run ci` runs: `format:check lint typecheck gen:check test build validate-examples contract`. `make go-ci` runs: `fmt-check vet lint vuln test-race build naming go-allowlist go-deps-check`. CI also runs `npm run check:ledger` (every deleted Go test file is mapped in `tools/test-ledger.json`).

Other targets: `make help` lists all of them.

| Need | Command |
|---|---|
| Dashboard tests | `npm test -w @openfactory/web` |
| Dashboard dev server | `make web-dev` |
| Regenerate every generated file | `npm run gen` (check: `npm run gen:check`) |
| Validate the example definitions | `npm run validate-examples` (needs `npm run build` first) |
| Sandbox images and smoke checks | `make sandbox-image sandbox-image-cloudflare images-check` |
| Live provider tests | `make e2e-providers` with the provider credentials set |
| microVM tests on a KVM host | `make e2e-firecracker` |
| Guest kernel | `make kernel` |

Tests that need a provider account, a Cloudflare account or KVM skip when their variables are not set.

## Change rules: regenerate together

Generated files are committed. CI fails when they are stale. Never edit generated files by hand.

| If you change | Also do | Check |
|---|---|---|
| `api/openapi.json` | `npm run gen:api`; add or change the handler and its entry in `apps/control-plane/src/http/routes.ts`; add a contract test. | `gen:check`, route-coverage test, `make openapi-check`, `npm run contract` |
| A definition field (`packages/definition`) | Update validation and the schema generator; `npm run gen:schemas gen:docs`; update `examples/` and the prose of `docs/definition-reference.md`. | `gen:check`, `validate-examples` |
| `schemas/runner/v1` | Edit the `internal/runnerproto` structs; `npm run gen:protos`; update `apps/control-plane/src/runner-api`. | `go test ./internal/runnerproto`, `gen:check`, contract runner section |
| `schemas/worker/v2` | Edit the `internal/sandbox/direct` frames; `npm run gen:protos`; update `apps/control-plane/src/do/workerhub`. | `go test ./internal/sandbox/direct`, `gen:check` |
| `web/src/` | Nothing generated is committed. | `npm test -w @openfactory/web`, `npm run build` |
| A DO schema | Add a migration file to that DO's `migrations/`. | migration test in `npm test` |
| `wrangler.jsonc` | `npm run gen:cf-types`; document new vars, secrets and bindings in `docs/self-hosting/configuration.md`. | `typecheck`, review |
| `packages/plugin-api` | Bump `PLUGIN_API_VERSION` on a breaking change; update `plugins/*`. | `typecheck`, plugin tests |
| `packages/sandbox` interface | Update all providers; run the conformance suite. | `npm test` |
| Redaction rules, failure mapping, run states | Update `schemas/testdata/*` and both implementations. | Go and TS tests |
| `deploy/sandbox/install.sh` | Rebuild images; `make boat-snapshot`; bump the image pin in the control-plane config. | `images` job |
| Runner protocol or runner release | `make runner-publish`; bump `RUNNER_VERSION` in the control plane. | session start refuses a mismatch |
| A CLI flag, env var, Worker var or secret | Update `docs/` in the same change. | review |

Rules:

- Never edit generated files by hand: `packages/api-client/src/schema.d.ts`, `packages/*/src/generated/`, `worker-configuration.d.ts`, `schemas/definition/v1/`, and the generated tables of `docs/definition-reference.md`.
- DO migrations: name files `NNNN_<name>.sql` with a new, higher number. Never edit a released migration; add a new one.
- A Go module that the removed server used must not come back (`make go-deps-check`). `go list ./...` must equal `tools/go-allowlist.txt` (`make go-allowlist`).
- `make openapi-baseline` runs only when a release is cut.

## Code conventions

TypeScript:

- Strict mode, ESM, `moduleResolution: Bundler`. Prettier formats; ESLint (`eslint.config.js`) lints. No `eval`: Workers forbid it.
- Errors: throw `OfError` from `@openfactory/core` with one of its codes (`invalid_request`, `authentication_required`, `not_authorized`, `resource_not_found`, `conflict`, `resource_unavailable`, ...). The HTTP layer maps the code to the status and the problem body.
- IDs: `newId(prefix)`, or `newRoutedId(prefix, token)` when the ID must route to a DO.
- DO rules:
  1. One `ctx.storage.transactionSync` per request or alarm step. It covers the state change, its outbox rows, its ledger row and its idempotency row.
  2. No `await` inside `transactionSync`, and no async callback. ESLint rejects both.
  3. No network I/O inside a logical transaction. Effects (other DOs, providers, forges, notifications) leave through the outbox (`apps/control-plane/src/lib/outbox`) after the commit.
- Cross-DO calls use the typed RPC of `apps/control-plane/src/contracts/do-messages.ts`.
- Tests sit next to the code (`*.test.ts`) or in the package `test/`. Control-plane tests run in the Workers runtime (`@cloudflare/vitest-plugin`). Dashboard tests are in `web/src/test/`. Use fetch mocks for external APIs. Write table-driven tests.

Go (runtime only):

- `gofmt -s` and `.golangci.yml` are the baseline.
- Errors: wrap the `internal/rtcore` sentinels (`ErrInvalid`, `ErrNotFound`, `ErrUnavailable`, `ErrForbidden`) with `%w` and context. Compare with `errors.Is`.
- Logs: `log/slog`. Pass `context.Context` as the first argument. IDs: `rtcore.NewID("prefix")`.
- Tests sit next to the code (`*_test.go`). Use `httptest` for HTTP peers and `internal/runner/runnertest` for a fake `/runner/v1` server.

Both:

- Read the code around your change and follow its style.
- Do not add a dependency (Go or npm) when the standard library or an existing dependency is enough. Explain each new one in the pull request. A pull request that changes `package-lock.json` names each added package and why.
- `@openfactory/control-plane` and `openfactory` are published to npm. Their exports are a public API: change them only on purpose.

## Docs and writing style

- Write plain technical English: short sentences, active voice, present tense, one term for one thing. Use the imperative for steps. Put a condition before its command.
- No marketing words, filler or emoji. Prefer tables and numbered steps.
- Every flag, env var, Worker var, field and command you name must exist. Verify with the code, `apps/control-plane/wrangler.jsonc`, `node apps/cli/dist/index.js <command> --help` or `go run ./cmd/openfactory <command> --help`.
- Keep relative links valid.
- `internal/naming/naming_test.go` checks `docs/`, `examples/`, `web/src/`, `apps/cli/`, `apps/control-plane/src/`, `deploy/sandbox/`, `deploy/machine/` and the runtime help text against the glossary ([docs/overview.md](docs/overview.md#glossary)). Read its `forbidden` list before you write user text. Run `make naming`.
- Use the machine name `openfactory`. The test rejects the old plural form. Write `workerHost: server`, never `workerHost: local`.

## Security rules

- Never commit secrets, tokens, private keys, real credentials, `.dev.vars` or `.db` files. Test values must be clearly fake.
- Never put personal names, personal emails or local machine paths in the repository.
- Do not weaken sandbox, network policy, authorization or redaction defaults. If a default must change, add tests that show the new behavior, and say so in the pull request. The restricted-policy capability gate stays on.
- CLI help must not show secret values from the environment (`apps/cli/test/cli.test.ts`, `cmd/openfactory/help_test.go`).
- Report vulnerabilities as described in [SECURITY.md](SECURITY.md), not in public issues.

## Pull requests

- One topic per pull request. Open an issue first for a large change.
- Add or update tests for each behavior change. Update `docs/` when behavior, flags, env vars, Worker vars, secrets or definition fields change.
- `make ci` must pass.
- Commit subject: imperative, 72 characters or less, prefixed with the package or area, for example `engine: retry claim on conflict`. The body tells what changed and why. Link the issue.
- Fill in `.github/pull_request_template.md`: what changed, how you tested it, what remains.
