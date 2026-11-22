import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The production build is written to web/dist and served by the control plane
// Worker (static assets). `npm run dev` proxies API and auth calls to a local
// `wrangler dev` control plane (OF_DEV_BACKEND, default http://localhost:8787).
declare const process: { env: Record<string, string | undefined> };
const backend = process.env.OF_DEV_BACKEND ?? "http://localhost:8787";

export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "dist",
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
});
