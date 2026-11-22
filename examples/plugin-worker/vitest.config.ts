import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Tests run in Node. "cloudflare:workers" is replaced by a small stand-in that the
// plugin API package ships for this purpose.
export default defineConfig({
  resolve: {
    alias: {
      "cloudflare:workers": fileURLToPath(
        new URL("../../packages/plugin-api/src/testing/cloudflare-workers-stub.ts", import.meta.url),
      ),
    },
  },
  test: { root: fileURLToPath(new URL(".", import.meta.url)), include: ["src/**/*.test.ts"] },
});
