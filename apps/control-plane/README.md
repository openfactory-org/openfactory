# @openfactory/control-plane

The Open Factory control plane: one Cloudflare Worker with Durable Objects (DO), R2, a Queue and a Cron trigger. See `.owner/cloudflare-rearchitecture-plan.md` for the design.

## Layout

| Path | Contents |
|---|---|
| `wrangler.jsonc` | Worker config: DO classes (`exports`), R2 `DATA`, Queue `JOBS`, Cron, Static Assets (`../../web/dist`), service bindings, vars, required secrets. Frozen at C0. |
| `src/index.ts` | `fetch` (probes, `/api/v1` router, other server paths, assets), `scheduled`, `queue`. Exports every DO class. |
| `src/contracts/env.ts` | `Env`: every binding, var and secret. Frozen at C0. |
| `src/contracts/do-messages.ts` | DO RPC interfaces, outbox kinds and payloads, Queue messages. Frozen at C0. |
| `src/http/routes.ts` | Every operation of `api/openapi.json` with its minimum role and handler. Frozen at C0. |
| `src/http/router.ts`, `src/http/handler.ts` | Path matcher and handler types. |
| `src/api/<resource>.ts` | REST handlers, one file per owner. Stubs answer 501 until the owner fills them in. |
| `src/do/<name>/index.ts` | DO classes: `team`, `work`, `dispatch`, `router`, `index`, `ingress`, `workerhub`, `benchmark`. |
| `src/do/<name>/migrations/` | `NNNN_<name>.sql`, applied in order in the constructor. |
| `test/` | Tests in the Workers runtime (`@cloudflare/vitest-plugin`). |

## Commands

Run from the repository root:

```bash
npx vitest run --project control-plane              # tests
npx tsc -b apps/control-plane                        # type check
npx wrangler deploy --dry-run -c apps/control-plane/wrangler.jsonc
npx wrangler dev -c apps/control-plane/wrangler.jsonc
npm run cf-types -w @openfactory/control-plane       # writes src/worker-configuration.d.ts
```

## Package

`npm run build -w @openfactory/control-plane` writes the npm package to `dist/`: ESM bundles of `src/index.ts` and `src/builder.ts` (`dist/index.js`, `dist/builder.js`, `dist/chunks/`) and type declarations (`dist/dts/`). The private workspace packages are bundled, and their declarations are in `dist/dts/vendor/`. The `dependencies` of `package.json` stay external. The build then runs `wrangler deploy --dry-run` into `.wrangler/dry-run`. A custom build imports the package and has its own `wrangler.jsonc`; see `examples/control-plane-builder`.

## Rules

- One `ctx.storage.transactionSync` per request or alarm step. No `await` inside it. Effects leave through the outbox.
- Errors are `OfError` from `@openfactory/core`. IDs come from `newId` and `newRoutedId`.
- A change to `wrangler.jsonc`, `src/contracts/*` or `src/http/routes.ts` is a contract change: ask the orchestrator.
