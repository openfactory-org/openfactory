# Control plane builder example

This example is a custom control plane Worker. It imports
`@openfactory/control-plane`, adds an HTTP route and a Cron hook, and exports the
result as its own Worker. Use this shape to ship your own build of the control
plane, for example with a private integration.

## What it shows

| Extension point | Function | Example |
| --- | --- | --- |
| HTTP route outside `/api/v1` | `registerServerRoute(prefix, handler)` | `GET /plugins/hello` answers with a text line. |
| Cron work | `registerCronHook(hook)` | A hook that logs once per minute. |

A route handler returns a `Response`, or `null` to pass the request on. The
longest matching prefix runs first. Routes must start with a path that the
control plane sends to its server handlers (for example `/plugins/`, `/mcp` or
`/integrations/`).

The module also re-exports `@openfactory/control-plane`. Wrangler needs the
Durable Object classes (`TeamDO`, `WorkDO` and the others) as exports of the
Worker module.

## Build and run

1. Copy `apps/control-plane/wrangler.jsonc` to this directory.
2. In the copy, set `name`, set `main` to `src/index.ts`, and change the
   `assets.directory` path to your dashboard build.
3. Set the required secrets from the `secrets` block of the file, for example
   with `wrangler secret put OF_SECRET_KEY`.
4. Run the Worker:

```bash
npm install
npm run dev
curl http://localhost:8787/plugins/hello
```

5. Deploy it with `npm run deploy`.

## Type check

```bash
npm run typecheck
```
