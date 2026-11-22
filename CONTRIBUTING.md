# Contributing to Open Factory

This page tells you how to set up the project, run the checks, and send a change. The detailed rules for each kind of change are in [AGENTS.md](AGENTS.md). They apply to human and AI contributors.

## Ground rules

- Open an issue before you start a large change. Describe the problem and the design you propose.
- Keep each pull request to one topic.
- Every change must keep `make ci` green.
- Follow the [Code of Conduct](CODE_OF_CONDUCT.md).
- Report security problems privately. See [SECURITY.md](SECURITY.md).

## Prerequisites

| Tool | Version | Used by |
|---|---|---|
| Node.js with npm | 24 or later (`.nvmrc`) | The control plane, the CLI, the dashboard and every TypeScript check |
| Go | The `go` line in `go.mod` | The runtime `openfactory-runtime`: build and tests |
| `git` | Any recent version | Build and tests |
| `wrangler` | Installed with `npm ci` | `make dev`, `wrangler deploy` |
| `golangci-lint` | v2 (CI uses the latest release) | `make lint`. See the [install page](https://golangci-lint.run/welcome/install/). |
| `govulncheck` | Latest | `make vuln`. Install with `go install golang.org/x/vuln/cmd/govulncheck@latest`. |
| `shellcheck` | Any recent version | `make shellcheck`. Install it with your package manager. |
| Docker or Podman | Optional | `make images-check` and the sandbox images |
| A Linux host with KVM | Optional | `make e2e-firecracker`, the microVM worker tests |

`make openapi-check` runs `oasdiff` with `go run`. You do not install it.

## Fast loop

```bash
git clone https://github.com/openfactory-org/openfactory.git
cd openfactory
npm ci
npm test            # TypeScript unit tests
make test           # Go unit tests
make dev            # the control plane with wrangler dev, on http://localhost:8787
```

To test one TypeScript package, run `npx vitest run --project <name>`. To test one Go package, run `go test ./internal/<package>/ -count=1`.

`make dev` keeps its data in `.wrangler/` and reads variables and secrets from `apps/control-plane/.dev.vars`. To run agents locally, start a development process worker with `make dev-worker`. Both are for development only. See [Quickstart](docs/quickstart.md) and [Local testing](docs/self-hosting/local-testing.md).

## The full gate: `make ci`

`make ci` runs each check of `.github/workflows/ci.yml`. It stops at the first failure. Run `make -k ci` to see all failures.

| Target | What it checks |
|---|---|
| `node-ci` | `npm ci`, then `npm run ci`: format, lint, type check, generated files, unit tests, build, example validation and the contract suite. |
| `go-ci` | Go format, `go vet`, `golangci-lint`, `govulncheck`, race tests, build, the naming test and the Go package list. |
| `shellcheck` | Every tracked shell script. |
| `openapi-check` | Breaking REST API changes against the baseline (report only, unless `OPENAPI_ENFORCE=1`). |

Run `make help` to list every target.

## Generated files

Do not edit these files by hand. Change the source, run the command, and commit both.

| Generated file | Source | Command |
|---|---|---|
| `schemas/definition/v1/` | The definition types in `packages/definition` | `npm run gen:schemas` |
| Field tables in `docs/definition-reference.md` | The JSON Schemas | `npm run gen:docs` |
| `packages/api-client/src/schema.d.ts` | `api/openapi.json` | `npm run gen:api` |
| `packages/runner-proto`, `packages/worker-proto` generated types | `schemas/runner/v1`, `schemas/worker/v2` | `npm run gen:protos` |
| `worker-configuration.d.ts` | `apps/control-plane/wrangler.jsonc` | `npm run gen:cf-types` |
| `api/openapi.v1.baseline.json` | `api/openapi.json` | `make openapi-baseline` (maintainers, when a release is cut) |

`npm run gen` runs them all, and `npm run gen:check` fails when a generated file is stale.

## Project layout

| Directory | Contents |
|---|---|
| `apps/control-plane` | The Cloudflare Worker and its Durable Objects, with `wrangler.jsonc` |
| `apps/cli` | The `openfactory` command |
| `packages/` | Shared packages: core types, definition, API client, protocols, sandbox contract, plugin API |
| `providers/` | Sandbox providers: `cloudflare-sandbox` (a Worker), `daytona` and `boatdev` (libraries) |
| `plugins/` | Plugin Workers: GitLab and Azure DevOps |
| `web/` | React dashboard source |
| `cmd/`, `internal/` | The Go runtime `openfactory-runtime`: runner, egress proxy, worker agent |
| `deploy/` | The sandbox image and bootstrap, and the worker host install files |
| `schemas/` | JSON Schemas of the definition, the runner protocol and the worker protocol |
| `api/` | The OpenAPI document and its baseline |
| `tests/` | The contract suite and the live end-to-end tests |
| `examples/` | Example factory definitions and TypeScript examples |
| `docs/` | User documentation |

## Code conventions

- TypeScript is strict ESM. Use the `OfError` codes of `packages/core` for errors and `newId` for IDs.
- In a Durable Object, use one `transactionSync` for each request or alarm step, with no `await` and no network call inside it. Effects go through the outbox.
- Go code uses `log/slog` and passes `context.Context` as the first argument.
- Write tests next to the code. Live checks skip when their variables are not set.
- Do not add a dependency when the standard library or an existing dependency is sufficient.
- If you change a definition field, update the validation code, the JSON Schemas, the examples and [docs/definition-reference.md](docs/definition-reference.md) in the same pull request.

## Documentation

Documentation lives in `docs/`.

- Use the imperative for steps: "Run the migration."
- Put a condition before its command: "If the build fails, read the log."
- Use one term for one thing. `internal/naming/naming_test.go` rejects terms that the glossary replaced.
- Make sure that every flag, environment variable and field you name exists in the code.

## Pick work

- Issues with the `good first issue` label are small and well defined.
- Issues with the `help wanted` label are open for anyone.
- Comment on the issue before you start, so that two people do not do the same work.

## Branches and pull requests

1. Fork the repository and create a branch from `main`.
2. Make the change. Add or update tests.
3. Run `make ci`.
4. Push the branch and open a pull request against `main`.
5. Fill in the pull request template.
6. Answer the review comments.

A maintainer merges the pull request when CI is green and one maintainer approves it.

## Commits

- Write the subject in the imperative, 72 characters or less. Start with the package or area: `engine: retry claim on conflict`.
- In the body, tell what changed and why.
- Link the issue that the change closes: `Closes #123`.

## What reviewers check

- The change does one thing and the description explains why.
- Tests cover the new behavior, and `make ci` passes.
- Generated files are regenerated, not edited by hand.
- Docs match the change: flags, environment variables, definition fields and API.
- No breaking change to the definition schema `v1`, the REST API `/api/v1` or the worker protocol without a discussion in an issue first. See [GOVERNANCE.md](GOVERNANCE.md).
- No secrets, personal data or local paths in code, tests or logs.

## Releases

Maintainers push a `vX.Y.Z` tag. The release workflow builds the `openfactory-runtime` binaries with GoReleaser, the Firecracker guest kernel and the sandbox images, and publishes the npm packages `openfactory` and `@openfactory/control-plane`. Changes are recorded in [CHANGELOG.md](CHANGELOG.md).

## License

When you submit a contribution, you license it under the [Apache License 2.0](LICENSE), as section 5 of the license states.
