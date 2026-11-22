#!/usr/bin/env node
/* global setTimeout, setInterval, clearInterval */
// Runner stand-in for the go target of the contract suite. The Go server's
// local sandbox backend starts it (OF_RUNNER_COMMAND) once per session with the
// task variables in the environment. It writes those variables to
// $OF_CONTRACT_RENDEZVOUS_DIR/<run>.<session>.json, so that a test can drive
// /runner/v1 itself as a fake runner. Control files written by the test:
//   <run>.<session>.exec.json  {"command": [...], "env": {...}}: run this
//                              command (for example the real Go runner) with
//                              the task environment, then exit with its code.
//   <run>.<session>.exit       exit now.
// It also exits on SIGTERM/SIGINT (the backend ends the task), when its parent
// (the server) exits, and after
// OF_CONTRACT_SHIM_MAX_SECONDS (default 900).
import { spawn } from "node:child_process";
import { existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const dir = process.env.OF_CONTRACT_RENDEZVOUS_DIR;
const runId = process.env.OF_RUN_ID ?? "";
const session = process.env.OF_SESSION ?? "";
if (!dir || !runId || !session) {
  console.error("runner-shim: OF_CONTRACT_RENDEZVOUS_DIR, OF_RUN_ID and OF_SESSION are required");
  process.exit(2);
}
const base = join(dir, `${runId}.${session}`);
// Only the task variables; the server's own keys are in the environment too.
const TASK_VARS =
  /^OF_(CONTROL_URL|RUN_ID|SESSION|BOOTSTRAP_TOKEN|RUNNER_[A-Z_]+|EGRESS_[A-Z_]+|SNAPSHOT_DECLARATIONS)$/;
const taskEnv = {};
for (const [k, v] of Object.entries(process.env)) {
  if (TASK_VARS.test(k) && k !== "OF_RUNNER_COMMAND" && v !== undefined) taskEnv[k] = v;
}
const record = { run_id: runId, session: Number(session), env: taskEnv, cwd: process.cwd(), pid: process.pid };
writeFileSync(`${base}.json.tmp`, JSON.stringify(record));
renameSync(`${base}.json.tmp`, `${base}.json`);

let child;
const stop = (code) => {
  if (child && child.exitCode === null) child.kill("SIGTERM");
  process.exit(code);
};
process.on("SIGTERM", () => stop(0));
process.on("SIGINT", () => stop(0));
const maxMs = Number(process.env.OF_CONTRACT_SHIM_MAX_SECONDS ?? "900") * 1000;
setTimeout(() => stop(0), maxMs).unref();

const parent = process.ppid;
const timer = setInterval(() => {
  if (existsSync(`${base}.exit`)) stop(0);
  // The server went away (orphaned): stop too.
  if (process.ppid !== parent) stop(0);
  if (!child && existsSync(`${base}.exec.json`)) {
    const spec = JSON.parse(readFileSync(`${base}.exec.json`, "utf8"));
    const [cmd, ...args] = spec.command;
    child = spawn(cmd, args, {
      env: { ...process.env, ...(spec.env ?? {}) },
      cwd: spec.cwd ?? process.cwd(),
      stdio: "inherit",
    });
    child.on("exit", (code, signal) => {
      clearInterval(timer);
      process.exit(code ?? (signal ? 1 : 0));
    });
  }
}, 150);
