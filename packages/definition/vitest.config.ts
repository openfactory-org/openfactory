import { defineProject } from "vitest/config";

export default defineProject({
  test: {
    name: "definition",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
  },
});
