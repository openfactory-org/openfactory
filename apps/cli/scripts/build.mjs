// Bundles the CLI into dist/index.js (one ESM file with a shebang).
// The workspace packages are TypeScript sources with bundler-style imports, so
// tsc cannot emit a runnable tree. esbuild resolves them and strips the types;
// tsc (npm run typecheck) checks the types.
import { chmodSync, mkdirSync } from "node:fs";
import { build } from "esbuild";
import { fileURLToPath, URL } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
mkdirSync(`${root}dist`, { recursive: true });
await build({
  entryPoints: [`${root}src/index.ts`],
  outfile: `${root}dist/index.js`,
  bundle: true,
  platform: "node",
  format: "esm",
  target: "node24",
  sourcemap: true,
  banner: {
    js: "#!/usr/bin/env node\nimport { createRequire as __cr } from 'node:module';\nconst require = __cr(import.meta.url);",
  },
  logLevel: "warning",
});
chmodSync(`${root}dist/index.js`, 0o755);
