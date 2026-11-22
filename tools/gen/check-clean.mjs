#!/usr/bin/env node
// Fails when generated paths differ from the committed tree (run after `npm run gen`).
import { execFileSync } from "node:child_process";

const GENERATED = [
  "packages/api-client/src/schema.d.ts",
  "packages/runner-proto/src/generated",
  "packages/worker-proto/src/generated",
  "packages/definition/src/generated",
  "schemas/definition/v1",
  "docs/definition-reference.md",
  "apps/control-plane/worker-configuration.d.ts",
];

const out = execFileSync("git", ["status", "--porcelain", "--untracked-files=all", "--", ...GENERATED], {
  encoding: "utf8",
});
if (out.trim() !== "") {
  console.error("Generated files are stale. Run `npm run gen` and commit the result:\n" + out);
  process.exit(1);
}
console.log("check-clean: generated files are up to date");
