// Root Vitest config: one project per workspace package. Each package has its own
// vitest.config.ts that sets the project name (the directory name, for example
// "core", "control-plane", "cloudflare-sandbox", "contract").
// Run one project: npx vitest run --project <name>
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // The Workers pool runs all files of a project in one runtime. A Durable
    // Object alarm or outbox delivery that one file started can run while the
    // worker of another file closes; the pool then loads the worker module for
    // that request and vitest reports EnvironmentTeardownError ("Closing rpc
    // while ... was pending"). It is not a test failure: the tests of the file
    // finished. Other unhandled errors still fail the run.
    onUnhandledError(error) {
      if (
        error.name === "EnvironmentTeardownError" &&
        /^\[vitest-worker\]: Closing rpc while "\w+" was pending/.test(error.message)
      )
        return false;
    },
    projects: [
      "packages/*/vitest.config.ts",
      "apps/*/vitest.config.ts",
      "providers/*/vitest.config.ts",
      "plugins/*/vitest.config.ts",
      "tests/*/vitest.config.ts",
      "web/vitest.config.ts",
    ],
  },
});
