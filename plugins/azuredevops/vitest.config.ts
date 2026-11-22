import { defineProject } from "vitest/config";

export default defineProject({
  test: {
    name: "azuredevops",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
