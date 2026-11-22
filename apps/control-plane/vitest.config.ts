import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineProject } from "vitest/config";

// Stand-ins for the provider and plugin Workers behind the service bindings.
// Owners replace them with the built Workers (scriptPath) when they need real calls.
const stubWorker = (name: string, entrypoint: string, id: string) => ({
  name,
  modules: true,
  compatibilityDate: "2026-11-15",
  script: `import { WorkerEntrypoint } from "cloudflare:workers";
export class ${entrypoint} extends WorkerEntrypoint { async id() { return "${id}"; } async ping() {} }
export default { fetch() { return new Response("stub", { status: 404 }); } };`,
});

export default defineProject({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      miniflare: {
        workers: [
          stubWorker("openfactory-provider-cloudflare-sandbox", "SandboxProvider", "cloudflare-sandbox"),
          stubWorker("openfactory-plugin-gitlab", "Plugin", "gitlab"),
          stubWorker("openfactory-plugin-azuredevops", "Plugin", "azuredevops"),
        ],
        // Clearly fake test values.
        bindings: {
          OF_SECRET_KEY: "test-only-not-a-real-key",
          OF_SIGNING_KEY: "test-only-not-a-real-signing-key",
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
