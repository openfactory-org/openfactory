// Contract suite. Run it with tools/contract/run.mjs, which starts a target and
// sets OF_CONTRACT_BASE_URL. Without that variable every suite skips.
import { defineProject } from "vitest/config";

export default defineProject({
  test: {
    name: "contract",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
    globalSetup: ["src/harness/global-setup.ts"],
    // One shared server: test files run one after the other.
    fileParallelism: false,
    testTimeout: 90_000,
    hookTimeout: 90_000,
  },
});
