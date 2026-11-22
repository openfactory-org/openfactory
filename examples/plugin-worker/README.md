# Example plugin Worker

A plugin is a Worker. The control plane binds it as a service binding and calls
its methods with typed RPC. This example serves two roles:

| Role | What it does |
|---|---|
| Responder for the provider `hello` | Logs every outbound message. |
| Ingress at `POST /plugins/hello/webhook` | Turns the JSON body `{"teamId": "...", "factoryId": "...", "text": "..."}` into one inbound event of provider `hello` and type `received`. |

## Files

| Path | Contents |
|---|---|
| `src/index.ts` | `class Plugin extends PluginWorker`. Every public method is an RPC method. |
| `src/index.test.ts` | Tests with the fake host of `@openfactory/plugin-api/testing`. |
| `wrangler.jsonc` | Worker configuration. The Worker has no public URL. |

## How a plugin works

1. Export `class Plugin extends PluginWorker` from `src/index.ts`. The entrypoint
   name must be `Plugin`.
2. Return the info from `info()`. Set `apiVersion` to `PLUGIN_API_VERSION`. The
   control plane refuses a plugin with another version.
3. Implement the methods of the roles you list in `roles`: `ForgePlugin`,
   `ResponderPlugin`, `IngressPlugin` or `SandboxPlugin` (see
   `packages/plugin-api/src/types.ts`).
4. Wrap each method body in `rpc(...)`. It turns thrown errors into error codes that
   the control plane understands.
5. Throw `notSupported(...)` from an optional method that you do not support. The
   message starts with `operation_not_supported`.

Rules:

- A payload is plain JSON. `Request`, `Response` and `ReadableStream` are allowed.
- A call carries at most 32 MiB. Stream larger bodies.
- The plugin holds no team secrets. For a forge call, the control plane resolves
  the credential and passes it in `ForgeTarget.token`. A webhook reads the secrets
  that an integration record names through `host.getSecret`.
- Plugin settings are the Worker vars and secrets of the plugin.
- Do not add public helper methods to the class. Every public method is callable
  over RPC. Use module functions or `#private` members.

## Test

From the repository root:

```bash
npx vitest run --config examples/plugin-worker/vitest.config.ts
npx tsc -p examples/plugin-worker
```

The tests run in Node. `@openfactory/plugin-api/testing` has `FakeHostApi`, which
checks the same secret rule as the control plane, and `mockFetch`, which fakes
outbound requests.

## Deploy

1. Deploy the Worker:

   ```bash
   npx wrangler deploy -c examples/plugin-worker/wrangler.jsonc
   ```

2. Bind it in `apps/control-plane/wrangler.jsonc`:

   ```jsonc
   { "binding": "PLUGIN_HELLO", "service": "hello-plugin", "entrypoint": "Plugin" }
   ```

3. Deploy the control plane. It finds every binding whose name starts with
   `PLUGIN_`, calls `info()` and uses the plugin when the API version matches.

4. Send a webhook:

   ```bash
   curl -X POST https://<your-control-plane>/plugins/hello/webhook \
     -H 'content-type: application/json' \
     -d '{"teamId": "<team id>", "text": "hello"}'
   ```
