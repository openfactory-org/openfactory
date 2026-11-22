// Helpers of the opt-in live scenarios. Each scenario skips unless its
// variables are set. Common variables:
//   OF_E2E_BASE_URL   the deployed control plane, for example https://factory.example.com
//   OF_E2E_API_KEY    an admin API key of that control plane
//   OF_E2E_ANTHROPIC_API_KEY  stored as the team secret ANTHROPIC_API_KEY when it is set
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe } from "vitest";
import type { Loose } from "./loose.ts";

const FIXTURES = resolve(dirname(fileURLToPath(import.meta.url)), "../fixtures");

export const live = {
  baseUrl: (process.env.OF_E2E_BASE_URL ?? "").replace(/\/+$/, ""),
  apiKey: process.env.OF_E2E_API_KEY ?? "",
};

/** describe() that runs only when every variable is set. */
export function describeLive(vars: string[], name: string, fn: () => void): void {
  const all = ["OF_E2E_BASE_URL", "OF_E2E_API_KEY", ...vars];
  const missing = all.filter((v) => !process.env[v]);
  if (missing.length === 0) describe(name, fn);
  else describe.skip(`${name} (set ${missing.join(", ")})`, fn);
}

export function env(name: string): string {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is not set`);
  return v;
}

/**
 * Reads a fixture tree (tests/e2e/fixtures/<name>) and replaces the
 * placeholders __NAME__ with the given values. A placeholder without a value
 * stays, and the server refuses the definition.
 */
export function fixture(name: string, values: Record<string, string> = {}): Record<string, string> {
  const root = join(FIXTURES, name);
  const files: Record<string, string> = {};
  const walk = (dir: string): void => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else {
        let text = readFileSync(p, "utf8");
        for (const [k, v] of Object.entries(values)) text = text.split(`__${k}__`).join(v);
        files[relative(root, p).split("\\").join("/")] = text;
      }
    }
  };
  walk(root);
  return files;
}

/** Gives the factory a unique name, so that a scenario can run again. */
export function renameFactory(files: Record<string, string>, name: string): Record<string, string> {
  const y = files["factory.yaml"] ?? "";
  return {
    ...files,
    "factory.yaml": y.replace(/^name:.*$/m, `name: ${name}`).replace(/^alias:.*$/m, `alias: ${name}`),
  };
}

export function uniq(prefix: string): string {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
}

export async function api<T = Loose>(
  method: string,
  path: string,
  body?: unknown,
  okStatus: number[] = [200, 201, 202, 204],
): Promise<T> {
  const res = await fetch(`${live.baseUrl}/api/v1${path}`, {
    method,
    headers: {
      authorization: `Bearer ${live.apiKey}`,
      accept: "application/json",
      ...(body === undefined ? {} : { "content-type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!okStatus.includes(res.status)) throw new Error(`${method} ${path}: status ${res.status}: ${text.slice(0, 500)}`);
  return (text ? JSON.parse(text) : undefined) as T;
}

/** An error that stops waitFor at once. */
export class Fatal extends Error {}

export async function waitFor<T>(
  what: string,
  fn: () => Promise<T | undefined | false>,
  timeoutMs: number,
  intervalMs = 3000,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let last: unknown;
  for (;;) {
    try {
      const v = await fn();
      if (v !== undefined && v !== false) return v as T;
    } catch (e) {
      if (e instanceof Fatal) throw e;
      last = e;
    }
    if (Date.now() > deadline)
      throw new Error(`timed out after ${timeoutMs} ms waiting for ${what}${last ? `: ${String(last)}` : ""}`);
    await new Promise((r) => setTimeout(r, intervalMs));
  }
}

/** Stores OF_E2E_ANTHROPIC_API_KEY as the team secret ANTHROPIC_API_KEY (once). */
export async function ensureModelSecret(): Promise<void> {
  const value = process.env.OF_E2E_ANTHROPIC_API_KEY;
  if (!value) return;
  await api("POST", "/secrets", { name: "ANTHROPIC_API_KEY", type: "anthropic_api_key", value }, [201, 409]);
}

export interface LiveRun {
  run_id: string;
  state: string;
  work_item_id?: string;
}

/** Waits until the run finished a turn (SUCCEEDED) and returns it. Fails on FAILED, ERROR or CANCELLED. */
export async function waitRunDone(runId: string, timeoutMs = 20 * 60_000): Promise<LiveRun> {
  return waitFor(
    `run ${runId} done`,
    async () => {
      const r = await api<LiveRun>("GET", `/runs/${runId}`);
      if (["FAILED", "ERROR", "CANCELLED"].includes(r.state)) throw new Fatal(`run ${runId} ended in ${r.state}`);
      return r.state === "SUCCEEDED" ? r : undefined;
    },
    timeoutMs,
  );
}
