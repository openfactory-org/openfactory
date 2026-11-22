// Projections of WorkDO state: the run timeline, the work item events, and
// the summaries that IndexDO and RouterDO keep of runs, work items and inbox
// items. Each projection is an outbox message, so it commits with the change.
import { newRoutedId } from "@openfactory/core";
import { dest } from "../../lib/outbox/dest";
import { iso, ms } from "../../engine/sessions";
import type { TimelineRow } from "../../engine/timeline";
import type { InboxItem, Run, WorkItem } from "../../engine/types";
import { runView } from "../../engine/view";
import type { Fx } from "./fx";

/** The root of a run tree (walks local parents). */
export function rootRunOf(fx: Fx, r: Run): string {
  let cur = r;
  for (let i = 0; i < 64 && cur.parent_run_id; i++) {
    const p = fx.repo.getRun(cur.parent_run_id);
    if (!p) break;
    cur = p;
  }
  return cur.id;
}

export function writeTimeline(fx: Fx, runId: string, rows: TimelineRow[]): void {
  for (const r of rows) {
    fx.repo.appendTimeline({
      event_id: r.eventId,
      run_id: runId,
      session: r.session,
      type: r.type,
      at: iso(r.atMs),
      ...(r.data ? { data: r.data } : {}),
    });
  }
}

export function timeline(
  fx: Fx,
  runId: string,
  session: number,
  type: string,
  eventId: string,
  data?: Record<string, unknown>,
  atMs?: number,
): void {
  writeTimeline(fx, runId, [{ eventId, type, session, atMs: atMs ?? fx.now(), ...(data ? { data } : {}) }]);
}

/** e.timeline: a work-item event, projected to IndexDO for metrics. */
export function workItemEvent(
  fx: Fx,
  wiId: string | undefined,
  kind: string,
  runId: string,
  actor: string,
  data: Record<string, unknown>,
  id?: string,
): void {
  if (!wiId) return;
  const wi = fx.repo.getWorkItem(wiId);
  if (!wi) return;
  const eventId = id ?? newRoutedId("wie", fx.token);
  const at = iso(fx.now());
  fx.repo.appendWorkItemEvent({ id: eventId, work_item_id: wiId, kind, run_id: runId, actor, data, created_at: at });
  fx.enqueue({
    dest: dest.index(fx.teamId),
    kind: "index.workItemEvent",
    msgId: `wie:${eventId}`,
    payload: { wiId, factoryId: wi.factory_id, eventId, kind, atMs: fx.now(), data },
  });
}

export function projectRun(fx: Fx, r: Run): void {
  fx.enqueue({
    dest: dest.index(fx.teamId),
    kind: "index.project",
    msgId: `proj:run:${r.id}:${r.version}`,
    payload: {
      entity: "run",
      id: r.id,
      version: r.version,
      summary: { ...runView(r, { publicUrl: fx.cfg.publicUrl }), root_run_id: rootRunOf(fx, r), work_do: fx.doName },
    },
  });
  fx.wakeRun(r.id);
}

export function projectWorkItem(fx: Fx, wi: WorkItem): void {
  fx.enqueue({
    dest: dest.index(fx.teamId),
    kind: "index.project",
    msgId: `proj:wi:${wi.id}:${wi.version}`,
    payload: { entity: "work_item", id: wi.id, version: wi.version, summary: { ...wi, work_do: fx.doName } },
  });
  // Router status lane: RouterDO ignores lower versions.
  fx.enqueue({
    dest: dest.router(fx.teamId),
    kind: "router.status",
    lane: "router",
    msgId: `rstat:${wi.id}:${wi.version}`,
    payload: {
      wiId: wi.id,
      factoryId: wi.factory_id,
      stage: wi.stage,
      completedMs: ms(wi.completed_at) ?? null,
      version: wi.version,
      keys: [wi.source_key ?? "", ...(wi.references ?? []).map((r) => r.key)].filter((k) => k !== ""),
      ...(wi.source_key ? { sourceKey: wi.source_key } : {}),
    },
  });
}

export function projectInbox(fx: Fx, it: InboxItem): void {
  fx.enqueue({
    dest: dest.index(fx.teamId),
    kind: "index.project",
    msgId: `proj:inbox:${it.id}:${it.version}`,
    payload: { entity: "inbox", id: it.id, version: it.version, summary: { ...it, owner: `work:${fx.token}` } },
  });
}
