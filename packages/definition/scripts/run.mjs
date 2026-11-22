// Runs a generator script (scripts/*.ts) under Node. The script imports
// @openfactory/core, whose bundler-style TypeScript (imports without a file
// extension, parameter properties) Node cannot run as is. esbuild bundles the
// script into a temporary file first, the way the CLI build does. The bundle
// keeps the script's own import.meta.url, so the script finds the repository
// files the same way.
//
// Usage: node scripts/run.mjs scripts/gen-schemas.ts [args...]
import { build } from "esbuild";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";

const entry = process.argv[2];
if (!entry) {
  console.error("usage: node scripts/run.mjs <script.ts> [args...]");
  process.exit(2);
}
const file = resolve(entry);
const dir = mkdtempSync(join(tmpdir(), "of-definition-gen-"));
process.on("exit", () => rmSync(dir, { recursive: true, force: true }));
const outfile = join(dir, "script.mjs");
await build({
  entryPoints: [file],
  outfile,
  bundle: true,
  platform: "node",
  format: "esm",
  target: "node24",
  logLevel: "warning",
  define: { "import.meta.url": JSON.stringify(pathToFileURL(file).href) },
  // Bundled CommonJS dependencies call require.
  banner: { js: `import { createRequire as __cr } from "node:module";\nconst require = __cr(${JSON.stringify(file)});` },
});
// The script reads its flags from process.argv: drop this runner from it.
process.argv.splice(1, 1);
await import(pathToFileURL(outfile).href);
