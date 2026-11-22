// The work item and its inbox items: creation, stage changes and the resolve
// of an inbox item. The work item completion is in lifecycle.ts, because it
// stops runs.
import { conflict, newRoutedId, notFound } from "@openfactory/core";
import { iso } from "../../engine/sessions";
import { truncate } from "../../engine/text";
import { stageTerminal, type InboxItem, type Run, type Stage, type WorkItem } from "../../engine/types";
import type { Fx } from "./fx";
import { projectInbox, projectWorkItem, workItemEvent } from "./projections";

export interface NewWorkItem {
  id?: string;
  factoryId: string;
  title: string;
  source?: WorkItem["source"];
  sourceKey?: string;
  automationId?: string;
  creatorId?: string;
}

export function createWorkItem(fx: Fx, n: NewWorkItem, actor: string, eventText?: string): WorkItem {
  const now = iso(fx.now());
  const wi: WorkItem = {
    id: n.id ?? newRoutedId("wi", fx.token),
    factory_id: n.factoryId,
    title: n.title,
    stage: "intake",
    source: n.source ?? {},
    ...(n.sourceKey ? { source_key: n.sourceKey } : {}),
    ...(n.automationId ? { automation_id: n.automationId } : {}),
    ...(n.creatorId ? { creator_id: n.creatorId } : {}),
    created_at: now,
    updated_at: now,
    version: 0,
  };
  fx.repo.createWorkItem(wi);
  fx.repo.setMeta("work_item_id", wi.id);
  workItemEvent(fx, wi.id, "stage_changed", "", actor, { to: "intake" });
  if (eventText) workItemEvent(fx, wi.id, "message_in", "", actor, { text: truncate(eventText, 4000) });
  projectWorkItem(fx, wi);
  return wi;
}

/** setStage: a work item stage change with its timeline and router status. */
export function setStage(fx: Fx, wiId: string, stage: Stage, actor: string, reason = ""): WorkItem {
  const wi = fx.repo.getWorkItem(wiId);
  if (!wi) throw notFound(`work item ${wiId}`);
  if (stageTerminal(wi.stage) && wi.stage !== stage) throw conflict(`work item is ${wi.stage}`);
  if (wi.stage === stage) return wi;
  const from = wi.stage;
  wi.stage = stage;
  wi.updated_at = iso(fx.now());
  if (stageTerminal(stage)) wi.completed_at = wi.updated_at;
  fx.repo.putWorkItem(wi);
  workItemEvent(fx, wi.id, "stage_changed", "", actor, { from, to: stage, ...(reason ? { reason } : {}) });
  projectWorkItem(fx, wi);
  return wi;
}

/** inferStage: move forward to a stage of a started stage agent; never backward. */
export function inferStage(fx: Fx, wiId: string, stage: string, actor: string): void {
  const order = ["intake", "triage", "planning", "awaiting_approval", "building", "reviewing", "handoff"];
  const wi = fx.repo.getWorkItem(wiId);
  if (!wi || stageTerminal(wi.stage)) return;
  if (order.indexOf(stage) > order.indexOf(wi.stage)) setStage(fx, wiId, stage as Stage, actor);
}

export function isSideRun(fx: Fx, r: Run): boolean {
  if (!r.work_item_id || r.parent_run_id) return false;
  const wi = fx.repo.getWorkItem(r.work_item_id);
  return !!wi && !!wi.root_run_id && wi.root_run_id !== r.id;
}

// ---------- inbox ----------

export function resolveInboxItem(fx: Fx, it: InboxItem, resolution: string, actor: string, via?: string): InboxItem {
  if (it.resolved) throw conflict("inbox item already resolved");
  it.resolved = true;
  it.resolution = resolution;
  it.resolved_by = actor;
  if (via) it.resolved_via = via;
  it.resolved_at = iso(fx.now());
  fx.repo.putInbox(it);
  projectInbox(fx, it);
  return it;
}

/**
 * resumesRun (workitems.go): whether a resolve sends the resolution to the
 * run. A handoff, an information-only item, a failure notice, a spend limit
 * and the review of a self-improvement pull request are bookkeeping.
 */
export function resumesRun(it: Pick<InboxItem, "kind" | "options">): boolean {
  switch (it.kind) {
    case "handoff":
    case "answered":
    case "failure":
    case "spend_limit":
      return false;
    case "review":
      return (it.options ?? []).length > 0;
  }
  return true;
}

/**
 * actorLabel (workitems.go): a readable name of an actor for text that people
 * and the model read, never a raw user ID. name is the user's name or email
 * when the caller looked it up.
 */
export function actorLabel(actor: string, name?: string): string {
  const n = (name ?? "").trim();
  if (n) return n;
  const a = actor.trim();
  if (!a || a === "api") return "a team member";
  const i = a.indexOf(":");
  if (i < 0) return a.startsWith("usr_") ? "a team member" : a;
  switch (a.slice(0, i)) {
    case "user":
      return "a team member";
    case "webhook":
    case "automation":
    case "trigger":
    case "schedule":
      return "an automation";
    case "run":
    case "agent":
    case "system":
      return "the factory";
  }
  return a;
}

/** The follow-up message of a resolve (ResolveInbox). */
export function resolutionMessage(it: Pick<InboxItem, "kind" | "title">, who: string, resolution: string): string {
  switch (it.kind) {
    case "approval":
      return `[human decision] Approval request ${JSON.stringify(it.title)} was resolved by ${who}: ${resolution}\n\nIf approved, continue with the next stage. If rejected or changes were requested, revise accordingly and ask again before implementation.`;
    case "question":
      return `[human answer] ${who} answered your question ${JSON.stringify(it.title)}:\n\n${resolution}`;
  }
  return `[human response] ${who} responded to ${JSON.stringify(it.title)} (${it.kind}):\n\n${resolution}`;
}
