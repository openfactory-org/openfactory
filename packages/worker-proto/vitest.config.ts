import { defineProject } from "vitest/config";

export default defineProject({
  test: {
    name: "worker-proto",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
