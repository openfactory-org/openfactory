import { defineProject } from "vitest/config";

export default defineProject({
  test: {
    name: "gitlab",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
