#!/usr/bin/env node
// Fails when a Go test file that existed at tag go-final is gone from the tree
// and has no entry in tools/test-ledger.json, or when an entry is still "pending".
// Usage: node tools/test-ledger-check.mjs [--base <ref>] [--allow-pending]
// An entry is the replacing TS test path, "kept", or "dropped: <reason>".
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";

const args = process.argv.slice(2);
const opt = (name, fallback) => {
  const i = args.indexOf(name);
  return i >= 0 ? args[i + 1] : fallback;
};
const base = opt("--base", "go-final");
const allowPending = args.includes("--allow-pending");

const git = (...a) => execFileSync("git", a, { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
const atBase = git("ls-tree", "-r", "--name-only", base)
  .split("\n")
  .filter((f) => f.endsWith("_test.go"));

const ledger = JSON.parse(readFileSync(new URL("./test-ledger.json", import.meta.url), "utf8")).files ?? {};
let bad = false;

for (const f of atBase) {
  if (existsSync(f)) continue; // still in the tree: the Go runtime keeps it
  const entry = ledger[f];
  if (entry === undefined) {
    console.error(`missing from tools/test-ledger.json: ${f}`);
    bad = true;
  } else if (entry === "pending" && !allowPending) {
    console.error(`still pending in tools/test-ledger.json: ${f}`);
    bad = true;
  }
}
for (const f of Object.keys(ledger)) {
  if (!atBase.includes(f)) {
    console.error(`ledger entry for a file that is not a Go test at ${base}: ${f}`);
    bad = true;
  }
}
if (bad) process.exit(1);
console.log(`test-ledger-check: ${atBase.length} Go test files at ${base} are accounted for`);
