import { fileURLToPath } from "node:url";
import { defineProject } from "vitest/config";

export default defineProject({
  resolve: {
    alias: {
      "cloudflare:workers": fileURLToPath(
        new URL("../../packages/plugin-api/src/testing/cloudflare-workers-stub.ts", import.meta.url),
      ),
    },
  },
  test: {
    name: "azuredevops",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
