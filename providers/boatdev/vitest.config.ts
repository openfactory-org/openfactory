import { defineProject } from "vitest/config";

export default defineProject({
  test: {
    name: "boatdev",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
