import { defineProject } from "vitest/config";

export default defineProject({
  test: {
    name: "cloudflare-sandbox",
    include: ["src/**/*.test.ts", "test/**/*.test.ts"],
    passWithNoTests: true,
  },
});
