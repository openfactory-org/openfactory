import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineProject } from "vitest/config";

// Stand-ins for the provider and plugin Workers behind the service bindings.
// The plugin stubs answer info() (plugin API version 1) so that plugin
// discovery accepts them; every other plugin call fails. Owners replace them
// with the built Workers (scriptPath) when they need real calls.
const stubWorker = (name: string, entrypoint: string, id: string, info?: Record<string, unknown>) => ({
  name,
  modules: true,
  compatibilityDate: "2026-11-15",
  script: `import { WorkerEntrypoint } from "cloudflare:workers";
export class ${entrypoint} extends WorkerEntrypoint { async id() { return "${id}"; } async ping() {}${
    info ? ` async info() { return ${JSON.stringify(info)}; }` : ""
  } }
export default { fetch() { return new Response("stub", { status: 404 }); } };`,
});

const pluginInfo = (name: string, forge: string) => ({ name, version: "0.0.0-stub", apiVersion: 1, roles: ["forge", "ingress"], forgeProviders: [forge] });

export default defineProject({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      miniflare: {
        workers: [
          stubWorker("openfactory-provider-cloudflare-sandbox", "SandboxProvider", "cloudflare-sandbox"),
          stubWorker("openfactory-plugin-gitlab", "Plugin", "gitlab", pluginInfo("gitlab", "gitlab")),
          stubWorker("openfactory-plugin-azuredevops", "Plugin", "azuredevops", pluginInfo("azuredevops", "azure_devops")),
        ],
        // Clearly fake test values.
        bindings: {
          // base64 of "test-only-fake-secret-key-32byte" and "test-only-fake-signing-key-32byt".
          OF_SECRET_KEY: "dGVzdC1vbmx5LWZha2Utc2VjcmV0LWtleS0zMmJ5dGU=",
          OF_SIGNING_KEY: "dGVzdC1vbmx5LWZha2Utc2lnbmluZy1rZXktMzJieXQ=",
          OF_R2_ACCESS_KEY_ID: "test-only-access-key-id",
          OF_R2_SECRET_ACCESS_KEY: "test-only-secret-access-key",
          OF_PUBLIC_URL: "http://localhost",
        },
      },
    }),
  ],
  test: {
    name: "control-plane",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
  },
});
