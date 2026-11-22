// Event ID formulas of internal/dispatch/timeline.go. A retried write with
// the same ID adds no second row. Go uses nanoseconds; times here are ms
// scaled to ns so that the IDs keep their shape.
import type { RunStateInput as StateInput, RunStateResult as RunResult } from "@openfactory/core";
import { ms } from "./sessions";
import type { Run } from "./types";

const nanos = (t: string | undefined, fallback: string): string => {
  const v = ms(t) ?? ms(fallback) ?? 0;
  return (BigInt(v) * 1_000_000n).toString();
};

export const taskIdOf = (runId: string, session: number): string => `${runId}.${session}`;

export function queuedEventId(r: Pick<Run, "id" | "queued_at" | "created_at">): string {
  return `run.queued:${r.id}:${nanos(r.queued_at, r.created_at)}`;
}

export interface TimelineRow {
  eventId: string;
  type: string;
  session: number;
  atMs: number;
  data?: Record<string, unknown>;
}

/** timelineTransition: the rows of a run state change. */
export function transitionRows(
  r: Run,
  input: StateInput,
  res: RunResult,
  session: number,
  nowMs: number,
): TimelineRow[] {
  const rows: TimelineRow[] = [];
  switch (r.state) {
    case "queued": {
      let reason = "new";
      if (input.event === "session_lost" || res.retry) {
        reason = "retry";
        const code = res.failureCode || input.failureCode || "";
        rows.push({
          eventId: `run.retry_scheduled:${taskIdOf(r.id, session)}:${nanos(r.queued_at, r.created_at)}`,
          type: "run.retry_scheduled",
          session,
          atMs: nowMs,
          data: { failure_code: code, attempt: (input.priorRetries ?? 0) + 1 },
        });
      } else if (input.event === "message") {
        reason = "message";
      }
      rows.push({
        eventId: queuedEventId(r),
        type: "run.queued",
        session,
        atMs: ms(r.queued_at) ?? nowMs,
        data: { reason },
      });
      break;
    }
    case "blocked":
      rows.push({
        eventId: `run.blocked:${r.id}:${r.turns ?? 0}:${r.blocked_reason ?? ""}`,
        type: "run.blocked",
        session,
        atMs: nowMs,
        data: { blocked_reason: r.blocked_reason ?? "" },
      });
      break;
    case "succeeded":
    case "failed":
    case "error":
    case "cancelled": {
      // The engine cancels runs too; the timeline route builds run.cancelled from the row.
      if (r.state === "cancelled" && input.event === "cancel") break;
      const at = ms(r.finished_at) ?? nowMs;
      const row: TimelineRow = {
        eventId: `run.${r.state}:${r.id}:${r.turns ?? 0}:${nanos(r.finished_at, new Date(at).toISOString())}`,
        type: `run.${r.state}`,
        session,
        atMs: at,
      };
      if (r.failure_code) row.data = { failure_code: r.failure_code };
      rows.push(row);
      break;
    }
  }
  return rows;
}

export function sessionEndedRow(
  runId: string,
  session: number,
  reason: string,
  sandbox: string,
  nowMs: number,
): TimelineRow {
  const data: Record<string, unknown> = { reason };
  if (sandbox) data.sandbox_reason = sandbox;
  return { eventId: `session.ended:${taskIdOf(runId, session)}`, type: "session.ended", session, atMs: nowMs, data };
}
