// Pure parts of internal/dispatch/dispatch.go and claim.go.
import type { RunResult } from "./runstate";
import type { Run, RunSession } from "./types";

export const iso = (ms: number): string => new Date(ms).toISOString();
export const ms = (t: string | undefined | null): number | undefined => (t ? Date.parse(t) : undefined);

/** Dispatcher.applyResult: write a runstate result into a run. */
export function applyResult(r: Run, res: RunResult, nowMs: number): void {
  const now = iso(nowMs);
  r.state = res.state;
  if (res.state !== "blocked") delete r.blocked_reason;
  if (res.state !== "queued") delete r.queued_reason;
  switch (res.state) {
    case "failed":
    case "error":
      r.failure_code = res.failureCode ?? "";
      r.error = res.failureCode ?? "";
      r.finished_at = now;
      break;
    case "blocked":
      r.blocked_reason = res.blockedReason ?? "";
      break;
    case "succeeded":
    case "cancelled":
      r.finished_at = now;
      break;
    case "running":
      if (!r.started_at) r.started_at = now;
      delete r.error;
      delete r.failure_code;
      break;
    case "queued":
      r.queued_at = now;
      delete r.finished_at;
      delete r.error;
      delete r.failure_code;
      break;
  }
}

/** The longest text that a runner adds to the error of a failed run. */
export const MAX_FAILURE_MESSAGE = 8000;

/** setFailureMessage: "code: text" for a failed run whose error is the bare code. */
export function setFailureMessage(r: Run, msg: string | undefined): void {
  let m = (msg ?? "").trim();
  if (m === "" || !r.failure_code || (r.state !== "failed" && r.state !== "error")) return;
  if ((r.error ?? "") !== r.failure_code) return;
  const b = new TextEncoder().encode(m);
  if (b.length > MAX_FAILURE_MESSAGE) {
    let cut = MAX_FAILURE_MESSAGE;
    while (cut > 0 && (b[cut]! & 0xc0) === 0x80) cut--;
    m = new TextDecoder().decode(b.subarray(0, cut)) + "...";
  }
  r.error = r.failure_code + ": " + m;
}

/** computeSeconds: life of the sandbox from start (or assignment) to end. */
export function computeSeconds(s: Pick<RunSession, "assigned_at" | "started_at">, endMs: number): number {
  const from = ms(s.started_at) ?? ms(s.assigned_at);
  if (from === undefined || Number.isNaN(from) || endMs < from) return 0;
  return (endMs - from) / 1000;
}

/**
 * priorRetries: the ended sessions before `before`, newest first, that ended
 * "lost" or "not_started"; stops at the first other one.
 */
export function priorRetries(list: readonly Pick<RunSession, "session" | "state" | "end_reason">[], before: number): number {
  const sorted = [...list].sort((a, b) => a.session - b.session);
  let count = 0;
  for (let i = sorted.length - 1; i >= 0; i--) {
    const s = sorted[i]!;
    if (s.session >= before) continue;
    if (s.state !== "ended" || (s.end_reason !== "lost" && s.end_reason !== "not_started")) break;
    count++;
  }
  return count;
}

/** queueExpired: counted from queued_at, or created_at. */
export function queueExpired(r: Pick<Run, "created_at" | "queued_at">, nowMs: number, timeoutMs: number): boolean {
  const since = ms(r.queued_at) ?? ms(r.created_at) ?? nowMs;
  return nowMs - since > timeoutMs;
}

/** sortedUnique: sorted distinct non-empty values. */
export function sortedUnique(input: readonly string[]): string[] {
  return [...new Set(input.filter((v) => v !== ""))].sort();
}

/** Go duration strings used by the vars ("30m", "4h", "10m", "30s", "1h30m"). */
export function parseDuration(s: string | undefined, fallbackMs: number): number {
  if (!s) return fallbackMs;
  const re = /(\d+(?:\.\d+)?)(ms|h|m|s)/g;
  let total = 0;
  let matched = "";
  for (const m of s.trim().matchAll(re)) {
    const n = Number(m[1]);
    total += m[2] === "h" ? n * 3_600_000 : m[2] === "m" ? n * 60_000 : m[2] === "s" ? n * 1000 : n;
    matched += m[0];
  }
  if (matched !== s.trim() || total <= 0) return fallbackMs;
  return total;
}
