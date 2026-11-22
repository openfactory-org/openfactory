// REST views of engine rows (api/openapi.json Run, WorkItem). Pure.
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

/** The openapi Run: state in capital letters, phase in lower case, run_id. */
export function runView(r: Run, opts: { publicUrl?: string; latest?: RunSession; rootRunId?: string } = {}): Record<string, unknown> {
  const { id, state, ...rest } = r;
  const out: Record<string, unknown> = {
    run_id: id,
    id,
    ...rest,
    state: STATE_UPPER[state] ?? state.toUpperCase(),
    phase: state,
    title: r.name || r.agent_name || "",
    sandbox_running: !!opts.latest && opts.latest.state !== "ended",
    updated_at: r.finished_at ?? r.started_at ?? r.created_at,
  };
  if (opts.rootRunId) out.root_run_id = opts.rootRunId;
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
