// Root Vitest config: one project per workspace package. Each package has its own
// vitest.config.ts that sets the project name (the directory name, for example
// "core", "control-plane", "cloudflare-sandbox", "contract").
// Run one project: npx vitest run --project <name>
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    projects: [
      "packages/*/vitest.config.ts",
      "apps/*/vitest.config.ts",
      "providers/*/vitest.config.ts",
      "plugins/*/vitest.config.ts",
      "tests/*/vitest.config.ts",
    ],
  },
});
