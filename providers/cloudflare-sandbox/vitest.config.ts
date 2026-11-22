import { fileURLToPath } from "node:url";
import { defineProject } from "vitest/config";

// The unit tests run in Node with fakes for ctx.container and storage.
// "cloudflare:workers" resolves to a small stub so index.ts can be imported.
export default defineProject({
  resolve: {
    alias: { "cloudflare:workers": fileURLToPath(new URL("./test/cloudflare-workers-stub.ts", import.meta.url)) },
  },
  test: {
    name: "cloudflare-sandbox",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
