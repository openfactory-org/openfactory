// REST views of engine rows (api/openapi.json Run, WorkItem). Pure.
// Port of internal/api/views.go runView and statusMessage.
import { lookupFailure, runSourceAndRef, usageCoverage } from "@openfactory/core";
import { runUrl } from "./lifecycle";
import type { Run, RunSession } from "./types";

const STATE_UPPER: Record<string, string> = {
  queued: "QUEUED",
  pending: "PENDING",
  starting: "PENDING",
  claimed: "CLAIMED",
  running: "RUNNING",
  blocked: "BLOCKED",
  idle: "SUCCEEDED",
  succeeded: "SUCCEEDED",
  failed: "FAILED",
  error: "ERROR",
  cancelled: "CANCELLED",
};

/** statusMessage (views.go): nil when the run has no text, no failure code and no reason. */
export function statusMessage(
  r: Run,
): { message: string; failure_code?: string; retryable: boolean; detail?: string } | undefined {
  const detail: string[] = [];
  let retryable = false;
  const info = r.failure_code ? lookupFailure(r.failure_code) : undefined;
  if (info) {
    retryable = info.retry;
    detail.push("failure class: " + info.class);
  }
  if (r.state === "blocked" && r.blocked_reason) detail.push("blocked reason: " + r.blocked_reason);
  if (r.state === "cancelled" && r.cancelled_by) detail.push("cancelled by: " + r.cancelled_by);
  const message = r.error ?? "";
  const fc = r.failure_code ?? "";
  const d = detail.join(", ");
  if (!message && !fc && !d) return undefined;
  return { message, ...(fc ? { failure_code: fc } : {}), retryable, ...(d ? { detail: d } : {}) };
}

/** deriveTitle (views.go): a short title from the first non-empty line of a prompt. */
export function deriveTitle(prompt: string): string {
  for (const raw of prompt.split("\n")) {
    const line = raw
      .trim()
      .replace(/^[#*\->]+/, "")
      .trim();
    if (!line) continue;
    const chars = [...line];
    return chars.length <= 80 ? line : chars.slice(0, 79).join("") + "…";
  }
  return "Untitled task";
}

/** The openapi Run: state in capital letters, phase in lower case, run_id, source. */
export function runView(
  r: Run,
  opts: { publicUrl?: string; latest?: RunSession; rootRunId?: string } = {},
): Record<string, unknown> {
  const { id, state, triggered_by: _t, ...rest } = r;
  const { source, ref } = runSourceAndRef(r);
  let upd = r.created_at;
  for (const t of [r.started_at, r.finished_at]) if (t && t > upd) upd = t;
  const sm = statusMessage(r);
  const out: Record<string, unknown> = {
    run_id: id,
    id,
    ...rest,
    state: STATE_UPPER[state] ?? state.toUpperCase(),
    phase: state,
    title: r.name || deriveTitle(r.prompt ?? ""),
    source,
    ...(ref ? { source_ref: ref } : {}),
    ...(sm ? { status_message: sm } : {}),
    sandbox_running: !!opts.latest && opts.latest.state !== "ended",
    root_run_id: opts.rootRunId ?? id,
    depth: 0,
    active_child_runs: 0,
    session: opts.latest?.session ?? 0,
    usage: {
      ...(r.usage ?? { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 }),
      coverage: usageCoverage(r.usage ?? {}),
    },
    updated_at: upd,
  };
  // spawnable_by is [] (not null) in API output (api/openapi.json).
  if (r.config) out.config = { ...r.config, spawnable_by: r.config.spawnable_by ?? [] };
  if (opts.publicUrl) {
    out.url = runUrl(opts.publicUrl, id);
    out.run_url = out.url;
  }
  return out;
}

/** Parses a state filter (capital or lower case) into phases. */
export function phasesOf(state: string): string[] {
  const s = state.trim().toLowerCase();
  if (s === "succeeded") return ["succeeded", "idle"];
  if (s === "pending") return ["pending", "starting"];
  return [s];
}
