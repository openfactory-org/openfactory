# Local testing

Run the control plane on your workstation with `wrangler dev`, run the contract suite against it, and opt in to live checks of providers and microVM workers.

> **Warning:** A development control plane with development process workers runs agents on your host with no isolation. Use it for tests only.

## Run the control plane locally

You need Node.js 24 or later and Go (the version in `go.mod`) for the runtime.

```bash
npm ci
make dev
```

`make dev` runs `wrangler dev` on <http://localhost:8787>. It keeps Durable Object and R2 data in `.wrangler/` and reads variables and secrets from `apps/control-plane/.dev.vars`. A minimal file:

```text
OF_SECRET_KEY=<output of: openssl rand -base64 32>
OF_SIGNING_KEY=<output of: openssl rand -base64 32>
OF_R2_ACCESS_KEY_ID=dev
OF_R2_SECRET_ACCESS_KEY=dev
OF_SANDBOX_PROVIDER=local
OF_DEV_PROCESS_WORKERS=true
```

Create the first team, then start a development worker in a second terminal:

```bash
export OF_SERVER=http://localhost:8787
openfactory bootstrap --email you@example.com      # prints OF_API_KEY once
export OF_API_KEY=of_...
make dev-worker                                    # builds the runtime and connects a worker
```

`make dev-seed` fills the control plane with sample runs, work items and inbox items. `make web-dev` runs the dashboard dev server.

## Checks

| Need | Command |
|---|---|
| Every TypeScript check | `npm run ci` (`make node-ci` runs `npm ci` first) |
| Unit tests | `npm test` |
| Go unit tests | `make test` |
| Naming and glossary check | `make naming` |
| Contract suite against `wrangler dev` | `make contract` |
| Validate the example definitions | `make validate-examples` |
| Regenerate generated files | `make gen`, and `make gen-check` to verify |

### Contract suite

`npm run contract` starts `wrangler dev`, runs the suite in `tests/contract` against it with a runner shim, and stops it. The suite covers the REST API, the runner API, the engine and the worker protocol.

## Live checks

Live tests need real accounts or hosts. They skip when their variables are not set.

| Check | Variables | Command |
|---|---|---|
| Deployed control plane: issue to pull request | `OF_E2E_BASE_URL`, `OF_E2E_API_KEY`, `OF_E2E_ANTHROPIC_API_KEY` | `npx vitest run --project e2e` |
| `cloudflare-sandbox` | `OF_E2E_CLOUDFLARE_SANDBOX=1` | The same |
| `daytona` | `OF_TEST_DAYTONA_KEY` | The same |
| `boatdev` | `OF_TEST_BOAT_KEY` | The same |
| Restricted policy on a provider | `OF_E2E_ALLOWED_HOST`, `OF_E2E_BLOCKED_URL` | The same |
| microVM worker on a KVM host | `OF_E2E_FIRECRACKER=1` | `go test -tags e2e_firecracker ./internal/machine/...` as root |

Daytona and boat.dev checks use the provider credentials configured on the control plane that you test.

## Sandbox images

| Need | Command |
|---|---|
| Build the sandbox image | `make sandbox-image` |
| Build the Cloudflare image | `make sandbox-image-cloudflare` |
| Build the images and run the smoke checks | `make images-check` |
| Build the boat.dev snapshot | `make boat-snapshot` |

## Related pages

- [Configuration](configuration.md)
- [Workers](workers.md)
- [Self-hosted workers](../self-hosted-machines.md)
