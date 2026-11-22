import { defineConfig, mergeConfig } from "vitest/config";
import viteConfig from "./vite.config";

// The "web" project of the root Vitest config: jsdom, React Testing Library.
export default mergeConfig(
  viteConfig,
  defineConfig({
    test: {
      name: "web",
      environment: "jsdom",
      globals: true,
      setupFiles: ["./src/test/setup.ts"],
      css: false,
      include: ["src/**/*.test.{ts,tsx}"],
    },
  }),
);
