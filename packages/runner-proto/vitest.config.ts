import { defineProject } from "vitest/config";

export default defineProject({
  test: {
    name: "runner-proto",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
