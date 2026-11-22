// Builds the publishable package @openfactory/control-plane into dist/:
//   dist/index.js, dist/builder.js (+ shared chunks)  ESM bundles of src/index.ts
//                                                     and src/builder.ts
//   dist/dts/                                         type declarations
// The workspace packages (@openfactory/core, ...) are private, so the bundles
// contain them, and their declarations are copied to dist/dts/vendor/<name>/
// with the imports rewritten to relative paths. Third-party modules are the
// dependencies of package.json and stay external, as do cloudflare:* and node:*.
// The declarations come from `tsc -b` (npm run typecheck), which this script
// runs first.
import { execFileSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, posix, relative, sep } from "node:path";
import { fileURLToPath, URL } from "node:url";
import { build } from "esbuild";

const pkg = fileURLToPath(new URL("..", import.meta.url));
const repo = join(pkg, "..", "..");
const out = join(pkg, "dist");
const manifest = JSON.parse(readFileSync(join(pkg, "package.json"), "utf8"));

for (const p of ["index.js", "index.js.map", "builder.js", "builder.js.map", "chunks", "dts"]) {
  rmSync(join(out, p), { recursive: true, force: true });
}

await build({
  entryPoints: { index: join(pkg, "src/index.ts"), builder: join(pkg, "src/builder.ts") },
  outdir: out,
  chunkNames: "chunks/[name]-[hash]",
  bundle: true,
  splitting: true,
  format: "esm",
  platform: "neutral",
  target: "es2022",
  conditions: ["workerd", "worker", "browser"],
  mainFields: ["module", "main"],
  // The dependencies of package.json stay external: the consumer installs them.
  external: ["cloudflare:*", "node:*", ...Object.keys(manifest.dependencies ?? {})],
  sourcemap: true,
  logLevel: "warning",
});

// Declarations: tsc -b writes them under dist/types/<path from the repo root>.
execFileSync(process.execPath, [join(repo, "node_modules/typescript/bin/tsc"), "-b", pkg], { stdio: "inherit" });

/** Workspace packages that the declarations import: name -> src directory of its declarations. */
const vendors = new Map();
for (const dir of ["packages", "providers"]) {
  for (const name of readdirSync(join(repo, dir))) {
    const pj = join(repo, dir, name, "package.json");
    if (!existsSync(pj)) continue;
    const { name: id, exports = {} } = JSON.parse(readFileSync(pj, "utf8"));
    // tsc writes dist/types/src or, with rootDir at the repo root, dist/types/<dir>/<name>/src.
    const types = [join(repo, dir, name, "dist/types", dir, name, "src"), join(repo, dir, name, "dist/types/src")].find((d) => existsSync(d));
    if (typeof id === "string" && id.startsWith("@openfactory/") && types !== undefined) vendors.set(id, { types, exports });
  }
}

const dts = join(out, "dts");
const copyDts = (from, to) =>
  cpSync(from, to, { recursive: true, filter: (f) => statSync(f).isDirectory() || (f.endsWith(".d.ts") && !f.endsWith(".test.d.ts")) });
copyDts(join(pkg, "dist/types/apps/control-plane/src"), join(dts, "src"));
for (const [id, v] of vendors) copyDts(v.types, join(dts, "vendor", id.slice("@openfactory/".length)));

const files = (d) => readdirSync(d, { recursive: true }).map((f) => join(d, String(f))).filter((f) => f.endsWith(".d.ts"));
const toPosix = (p) => p.split(sep).join(posix.sep);
const used = new Set();
for (const f of files(dts)) {
  const src = readFileSync(f, "utf8");
  const next = src.replace(/(from |import\()(["'])([^"']+)\2/g, (m, kw, q, spec) => {
    let s = spec;
    const v = /^@openfactory\/([^/]+)(\/.*)?$/.exec(s);
    if (v !== null) {
      if (!vendors.has(`@openfactory/${v[1]}`)) throw new Error(`${relative(out, f)}: no declarations for ${s}`);
      used.add(v[1]);
      const exp = vendors.get(`@openfactory/${v[1]}`).exports[v[2] ? "." + v[2] : "."];
      if (typeof exp !== "string" || !exp.startsWith("./src/")) throw new Error(`${relative(out, f)}: cannot map ${s}`);
      const target = join(dts, "vendor", v[1], exp.slice("./src/".length).replace(/\.ts$/, ""));
      s = toPosix(relative(dirname(f), target));
      if (!s.startsWith(".")) s = "./" + s;
      s += ".js";
    } else if (s.startsWith(".")) {
      s = s.replace(/\.ts$/, ".js");
      if (!s.endsWith(".js") && !s.endsWith(".json")) s += ".js";
    }
    return `${kw}${q}${s}${q}`;
  });
  if (next !== src) writeFileSync(f, next);
}
for (const id of vendors.keys()) {
  if (!used.has(id.slice("@openfactory/".length))) rmSync(join(dts, "vendor", id.slice("@openfactory/".length)), { recursive: true, force: true });
}
mkdirSync(dts, { recursive: true });
