#!/usr/bin/env node
// Fails when a workstream branch changes paths that its workstream does not own.
// Usage: node tools/check-ownership.mjs [--ws <n>] [--base <ref>]
// Without --ws the workstream comes from the branch name refactor/ws-<n>-*.
// The owned paths are in tools/ownership.json: { "<ws>": ["path/prefix/", "file"] }.
// "frozen" lists the C0 contract files: no workstream branch may change them.
// A path owned by two workstreams is an error in itself.
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";

const args = process.argv.slice(2);
const opt = (name) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : undefined;
};
const git = (...a) => execFileSync("git", a, { encoding: "utf8" }).trim();

const mapFile = new URL("./ownership.json", import.meta.url);
if (!existsSync(mapFile)) {
  console.log("check-ownership: tools/ownership.json not found; nothing to check");
  process.exit(0);
}
const owners = JSON.parse(readFileSync(mapFile, "utf8"));
const owns = (prefix, path) => (prefix.endsWith("/") ? path.startsWith(prefix) : path === prefix);

// 1. No path prefix has two owners.
let bad = false;
const frozen = owners.frozen ?? [];
const entries = Object.entries(owners)
  .filter(([ws]) => ws !== "frozen")
  .flatMap(([ws, paths]) => paths.map((p) => [ws, p]));
for (const [wa, pa] of entries) {
  for (const [wb, pb] of entries) {
    if (wa < wb && (owns(pa, pb) || owns(pb, pa))) {
      console.error(`overlap: ${pa} (WS${wa}) and ${pb} (WS${wb})`);
      bad = true;
    }
  }
}

// 2. The branch touches only its own paths.
const branch = git("rev-parse", "--abbrev-ref", "HEAD");
const ws = opt("--ws") ?? /^refactor\/ws-(\d+)-/.exec(branch)?.[1];
if (ws !== undefined) {
  const base = opt("--base") ?? "c0";
  const mine = owners[ws] ?? [];
  const changed = git("diff", "--name-only", `${base}...HEAD`).split("\n").filter(Boolean);
  for (const f of changed) {
    if (frozen.some((p) => owns(p, f))) {
      console.error(`${f} is a frozen contract file: ask the orchestrator for a contract change`);
      bad = true;
    } else if (!mine.some((p) => owns(p, f))) {
      console.error(`WS${ws} does not own ${f}`);
      bad = true;
    }
  }
}
if (bad) process.exit(1);
console.log(`check-ownership: ok${ws === undefined ? " (no workstream branch)" : ` (WS${ws})`}`);
