import { fileURLToPath } from "node:url";
import { defineProject } from "vitest/config";

export default defineProject({
  resolve: {
    alias: {
      "cloudflare:workers": fileURLToPath(new URL("./src/testing/cloudflare-workers-stub.ts", import.meta.url)),
    },
  },
  test: {
    name: "plugin-api",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
