/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The production build is written to internal/webui/dist and embedded in the Go
// binary (//go:embed dist). `npm run dev` proxies API and auth calls to a local
// openfactory server (OF_DEV_BACKEND, default http://localhost:8080).
declare const process: { env: Record<string, string | undefined> };
const backend = process.env.OF_DEV_BACKEND ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
    assetsDir: "assets",
    sourcemap: false,
    chunkSizeWarningLimit: 600,
  },
  server: {
    proxy: {
      "/api": { target: backend, changeOrigin: false },
      "/auth": { target: backend, changeOrigin: false },
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
});
