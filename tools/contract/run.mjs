#!/usr/bin/env node
/* global fetch, setTimeout */
// Runs the black-box contract suite (tests/contract) against one target.
//
//   node tools/contract/run.mjs --target worker [--port N] [-- <vitest args>]
//   node tools/contract/run.mjs --target url --url http://127.0.0.1:8787 [--api-key K]
//
// worker  starts `wrangler dev -c apps/control-plane/wrangler.jsonc` on a free
//         port with development variables (local provider, a test master key,
//         a test signing key) and a fresh persistence directory.
// url     uses a control plane that is already running. The runner suites
//         need OF_SANDBOX_PROVIDER=local and OF_DEV_PROCESS_WORKERS=true there.
//
// Options:
//   --keep             keep the temporary directory and print its path
//   --report <file>    write the vitest JSON report to <file>
//   --real-runner      also run the opt-in test with the real Go runner
//   --log              stream the server log to stderr
//
// Exit code: the exit code of vitest (0 when every selected test passed).
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { createWriteStream, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../..");

const RUNNER_VERSION = "1.0.0-contract";

function parseArgs(argv) {
  const out = {
    target: "",
    port: 0,
    url: "",
    apiKey: "",
    keep: false,
    report: "",
    realRunner: false,
    log: false,
    rest: [],
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => {
      const v = argv[++i];
      if (v === undefined) throw new Error(`${a} needs a value`);
      return v;
    };
    if (a === "--") {
      out.rest = argv.slice(i + 1);
      break;
    } else if (a === "--target") out.target = next();
    else if (a === "--port") out.port = Number(next());
    else if (a === "--url") out.url = next();
    else if (a === "--api-key") out.apiKey = next();
    else if (a === "--report") out.report = next();
    else if (a === "--keep") out.keep = true;
    else if (a === "--real-runner") out.realRunner = true;
    else if (a === "--log") out.log = true;
    else if (a === "-h" || a === "--help") {
      console.log(
        readFileSync(fileURLToPath(import.meta.url), "utf8")
          .split("\n")
          .slice(1, 20)
          .join("\n"),
      );
      process.exit(0);
    } else throw new Error(`unknown argument ${a}`);
  }
  if (!["worker", "url"].includes(out.target)) throw new Error("--target must be worker or url");
  if (out.target === "url" && !out.url) throw new Error("--target url needs --url");
  return out;
}

function freePort() {
  return new Promise((res, rej) => {
    const s = createServer();
    s.unref();
    s.on("error", rej);
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => res(port));
    });
  });
}

async function waitReady(url, proc, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  let last = "";
  while (Date.now() < deadline) {
    if (proc && proc.exitCode !== null)
      throw new Error(`the server exited with code ${proc.exitCode} before it was ready`);
    try {
      const r = await fetch(url);
      if (r.status === 200) return;
      last = `status ${r.status}`;
    } catch (e) {
      last = String(e.cause?.code ?? e.message);
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`${url} not ready after ${timeoutMs} ms (${last})`);
}

function startProcess(cmd, args, opts, logFile, streamLog) {
  const log = createWriteStream(logFile);
  const p = spawn(cmd, args, { ...opts, stdio: ["ignore", "pipe", "pipe"], detached: process.platform !== "win32" });
  for (const s of [p.stdout, p.stderr]) {
    s.pipe(log, { end: false });
    if (streamLog) s.pipe(process.stderr, { end: false });
  }
  return p;
}

function stopProcess(p) {
  if (!p || p.exitCode !== null) return Promise.resolve();
  return new Promise((res) => {
    const done = () => res();
    p.once("exit", done);
    try {
      // The process group: wrangler starts workerd.
      process.kill(-p.pid, "SIGTERM");
    } catch {
      p.kill("SIGTERM");
    }
    setTimeout(() => {
      try {
        process.kill(-p.pid, "SIGKILL");
      } catch {
        // gone
      }
      res();
    }, 8000).unref();
  });
}

async function startWorker(args, tmp) {
  const port = args.port || (await freePort());
  const inspector = await freePort();
  const base = `http://127.0.0.1:${port}`;
  const envFile = join(tmp, "dev.env");
  const vars = {
    OF_PUBLIC_URL: base,
    OF_SANDBOX_PROVIDER: "local",
    OF_DEV_PROCESS_WORKERS: "true",
    OF_RUNNER_VERSION: RUNNER_VERSION,
    OF_LOG_LEVEL: "info",
    // Test keys, generated per run.
    OF_SECRET_KEY: randomBytes(32).toString("base64"),
    OF_SIGNING_KEY: randomBytes(32).toString("base64"),
    OF_R2_ACCESS_KEY_ID: "contract-test-access-key",
    OF_R2_SECRET_ACCESS_KEY: "contract-test-secret-key",
  };
  writeFileSync(
    envFile,
    Object.entries(vars)
      .map(([k, v]) => `${k}=${v}`)
      .join("\n") + "\n",
  );
  const wrangler = join(ROOT, "node_modules/.bin/wrangler");
  const p = startProcess(
    wrangler,
    [
      "dev",
      "-c",
      "apps/control-plane/wrangler.jsonc",
      "--ip",
      "127.0.0.1",
      "--port",
      String(port),
      "--inspector-port",
      String(inspector),
      "--env-file",
      envFile,
      "--persist-to",
      join(tmp, "state"),
      "--show-interactive-dev-session=false",
      "--log-level",
      "info",
    ],
    { cwd: ROOT, env: { ...process.env, CI: "1", WRANGLER_SEND_METRICS: "false" } },
    join(tmp, "server.log"),
    args.log,
  );
  await waitReady(`${base}/healthz`, p, 120_000);
  return { proc: p, base };
}

function summarize(reportFile, targetName) {
  if (!existsSync(reportFile)) return;
  const r = JSON.parse(readFileSync(reportFile, "utf8"));
  const failed = [];
  for (const f of r.testResults ?? []) {
    for (const t of f.assertionResults ?? []) {
      if (t.status === "failed") failed.push(`${f.name.replace(ROOT + "/", "")} > ${t.fullName}`);
    }
    if ((f.assertionResults ?? []).length === 0 && f.status === "failed")
      failed.push(`${f.name.replace(ROOT + "/", "")}: ${f.message?.split("\n")[0] ?? "failed"}`);
  }
  console.log(
    `\ncontract target=${targetName}: ${r.numPassedTests} passed, ${r.numFailedTests} failed, ` +
      `${r.numPendingTests + (r.numTodoTests ?? 0)} skipped, ${r.numTotalTests} total`,
  );
  if (failed.length) {
    console.log("failed:");
    for (const f of failed) console.log(`  - ${f}`);
  }
}

async function main() {
  const args = parseArgs(process.argv.slice(2));
  const tmp = mkdtempSync(join(tmpdir(), `of-contract-${args.target}-`));
  let server;
  let code;
  const cleanup = async () => {
    await stopProcess(server?.proc);
    if (!args.keep) rmSync(tmp, { recursive: true, force: true });
  };
  process.on("SIGINT", () => void cleanup().then(() => process.exit(130)));
  try {
    if (args.target === "worker") server = await startWorker(args, tmp);
    else server = { proc: undefined, base: args.url.replace(/\/+$/, "") };
    console.log(`contract: target ${args.target} ready at ${server.base} (temporary directory ${tmp})`);

    const reportFile = args.report ? resolve(args.report) : join(tmp, "report.json");
    const env = {
      ...process.env,
      OF_CONTRACT_BASE_URL: server.base,
      OF_CONTRACT_RUNNER_VERSION: RUNNER_VERSION,
    };
    if (args.apiKey) env.OF_CONTRACT_API_KEY = args.apiKey;
    if (args.realRunner) env.OF_CONTRACT_REAL_RUNNER = "1";
    const vitest = join(ROOT, "node_modules/.bin/vitest");
    const vargs = [
      "run",
      "--root",
      "tests/contract",
      "--reporter=default",
      "--reporter=json",
      `--outputFile.json=${reportFile}`,
      ...args.rest,
    ];
    code = await new Promise((res) => {
      const p = spawn(vitest, vargs, { cwd: ROOT, env, stdio: "inherit" });
      p.on("exit", (c) => res(c ?? 1));
    });
    summarize(reportFile, args.target);
    if (code !== 0) console.log(`server log: ${join(tmp, "server.log")}${args.keep ? "" : " (deleted; use --keep)"}`);
  } catch (e) {
    console.error(`contract: ${e.message}`);
    if (existsSync(join(tmp, "server.log"))) {
      console.error("--- server log (tail) ---");
      console.error(readFileSync(join(tmp, "server.log"), "utf8").split("\n").slice(-40).join("\n"));
    }
    code = 1;
  } finally {
    await cleanup();
  }
  process.exit(code);
}

main();
