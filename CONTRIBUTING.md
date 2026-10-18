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
| Go | The `go` line in `go.mod` (1.27.4) | Build and tests |
| `git` | Any recent version | Build; the `local` sandbox backend |
| Node.js with npm | 20 or later (CI uses 22) | `make web-build`, `make web-test`, `make web-check-api`, `make web-check` |
| `golangci-lint` | v2 (CI uses the latest release) | `make lint`. See the [install page](https://golangci-lint.run/welcome/install/). |
| `govulncheck` | Latest | `make vuln`. Install with `go install golang.org/x/vuln/cmd/govulncheck@latest`. The Makefile also finds it in `$(go env GOPATH)/bin`. |
| `shellcheck` | Any recent version | `make shellcheck`. Install it with your package manager. |
| Helm | 3 | `make helm-test`. See the [install page](https://helm.sh/docs/intro/install/). |
| Docker or Podman | Optional | `make images` and the local test stack |

`make openapi-check` runs `oasdiff` with `go run`. You do not install it.

## Fast loop

```bash
git clone https://github.com/openfactory-org/openfactory.git
cd openfactory
make build          # bin/openfactory
make test           # unit tests
make run-dev        # OF_DEV=true openfactory serve
```

To test one package, run `go test ./internal/<package>/ -count=1`.

Development mode uses SQLite in `~/.openfactory`, creates a secret key, logs you in to the dashboard automatically, and prints an admin API key one time. Agents run with the `local` sandbox backend, in git worktrees on your machine.

To run the server in a cluster, start the local test stack:

```bash
make local-up       # OrbStack Kubernetes (the default)
KUBE_CONTEXT=kind-of-local make local-up   # kind: creates the cluster when it does not exist
make local-check
```

The stack runs the server from the Helm chart, with PostgreSQL, the AWS emulator and the `kubernetes` sandbox backend. It is for tests only. See [Local testing](docs/self-hosting/local-testing.md). For the tests that need PostgreSQL or an S3-compatible store, see [Tests with PostgreSQL or S3](docs/self-hosting/local-testing.md#tests-with-postgresql-or-s3).

**Old development databases.** The schema starts at one baseline migration (`0001_baseline.sql`). A database from an earlier development build does not open: the server stops with an error about migration 1. Make the database again. For the development server, delete `~/.openfactory/openfactory.db` (or the file in `OF_DATABASE_URL`). For PostgreSQL, drop and create the database.

## The full gate: `make ci`

`make ci` runs each check of `.github/workflows/ci.yml`. It stops at the first failure. Run `make -k ci` to see all failures.

| Target | What it checks |
|---|---|
| `fmt-check` | Go code is formatted with `gofmt -s`. Run `make fmt` to fix. |
| `vet` | `go vet`. |
| `lint` | `golangci-lint` with `.golangci.yml`. |
| `vuln` | `govulncheck` on the code and its dependencies. |
| `shellcheck` | Every tracked shell script. |
| `test-race` | Unit tests with the race detector. |
| `build`, `plugins` | The binary and the bundled gRPC plugins. |
| `examples-go` | The Go example modules. |
| `validate-examples` | Every definition in `examples/`. |
| `schemas-check` | The definition JSON Schemas are current. |
| `docs-check` | The field tables of `docs/definition-reference.md` are current. |
| `helm-test` | `helm lint` and the chart render tests. |
| `openapi-check` | Breaking REST API changes against the baseline (report only, unless `OPENAPI_ENFORCE=1`). |
| `upgrade-test` | Database upgrade tests. |
| `web-test` | Dashboard unit tests (vitest). |
| `web-check-api` | `web/src/api/schema.d.ts` matches the OpenAPI spec. |
| `web-check` | The committed dashboard build in `internal/webui/dist` is current. |

Run `make help` to list every target.

## Generated files

Do not edit these files by hand. Change the source, run the command, and commit both.

| Generated file | Source | Command |
|---|---|---|
| `internal/definition/schemas/` | Definition types in `internal/definition` | `make schemas` |
| Field tables in `docs/definition-reference.md` | The JSON Schemas | `make docs-gen` |
| `web/src/api/schema.d.ts` | `internal/api/openapi.json` | `cd web && npm run gen:api` (also part of `make web-build`) |
| `internal/webui/dist/` | `web/` | `make web-build` |
| `pkg/plugin/pb/*.pb.go` | `proto/openfactory/plugin/` | `go generate ./pkg/plugin/pb` (needs `protoc`, `protoc-gen-go` and `protoc-gen-go-grpc`) |
| `api/openapi.v1.baseline.json` | `internal/api/openapi.json` | `make openapi-baseline` (maintainers, when a release is cut) |

## Project layout

Each directory under `internal/` is one package with one job. `internal/core` holds the shared types and interfaces. Other packages depend on `internal/core` and get their dependencies injected. `internal/server` connects everything.

| Directory | Contents |
|---|---|
| `cmd/openfactory` | The CLI and server entry point |
| `internal/` | Server packages |
| `pkg/` | Public Go packages: the SDK, the plugin SDK and the embeddable server (experimental before v1) |
| `proto/` | gRPC plugin protocol |
| `web/` | React dashboard source |
| `deploy/` | Helm chart, Dockerfiles, the local test stack |
| `images/` | Tool images for the agent harnesses |
| `plugins/` | Bundled gRPC plugins |
| `examples/` | Example factory definitions and Go modules |
| `docs/` | User documentation |

## Code conventions

- Use `log/slog` for logs. Pass `context.Context` as the first argument.
- Create IDs with `core.NewID("prefix")`.
- HTTP handlers expose `Register(mux *http.ServeMux)` and use Go method patterns, for example `"POST /api/v1/factories/{uid}/runs"`.
- Write table-driven tests. Use fakes for the `internal/core` interfaces. Use `httptest` for external APIs.
- Do not add a dependency when the standard library is sufficient.
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
- Docs match the change: flags, environment variables, definition fields, API and Helm values.
- No breaking change to the definition schema `v1`, the REST API `/api/v1` or the Helm values without a discussion in an issue first. See [GOVERNANCE.md](GOVERNANCE.md).
- No secrets, personal data or local paths in code, tests or logs.

## Releases

Maintainers push a `vX.Y.Z` tag. The release workflow builds the binaries with GoReleaser, publishes the images `ghcr.io/openfactory-org/openfactory` and `ghcr.io/openfactory-org/sandbox`, and pushes the Helm chart to `oci://ghcr.io/openfactory-org/charts`. Changes are recorded in [CHANGELOG.md](CHANGELOG.md).

## License

When you submit a contribution, you license it under the [Apache License 2.0](LICENSE), as section 5 of the license states.
