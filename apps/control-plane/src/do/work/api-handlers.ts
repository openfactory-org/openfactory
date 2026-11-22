// The REST operations that WorkDO.api serves, as a table of handlers. Each
// entry names the object whose factory authorizes the call (scope) and the
// role that its route needs; WorkDO.api authorizes before it runs a handler.
import { usageCoverage, conflict, invalid, newRoutedId, notFound, OfError, partitionOf } from "@openfactory/core";
import type { CallMeta, Role } from "../../contracts/do-messages";
import { iso } from "../../engine/sessions";
import {
  STAGES,
  stageTerminal,
  type InboxItem,
  type PullRequestRef,
  type RunSession,
  type Stage,
} from "../../engine/types";
import { approverDeps, canApprove, notAnApprover } from "./approvers";
import type { Env } from "../../contracts/env";
import {
  actorLabel,
  cancel,
  projectInbox,
  projectWorkItem,
  resumesRun,
  sendMessage,
  setStage,
  workItemEvent,
  type Fx,
} from "./engine";
import { answerInbox, completeWorkItem, linkPullRequest } from "./intake";
import type { Repo } from "./repo";

/** What a handler gets. */
export interface ApiCall {
  meta: CallMeta;
  params: Record<string, string>;
  body: unknown;
  /** params.runId, else params.id. */
  runId: string;
  /** A repository for reads outside a transaction. */
  repo: Repo;
  /** One transaction with an Fx. */
  txFx<T>(fn: (fx: Fx) => T): T;
  env: Env;
  teamId: string;
}

/**
 * The object of a call whose factory authorizes it (routeFactory of the Go
 * route wrapper): the run of params.runId (or params.id), the work item of
 * params.id, the inbox item of params.id, or the target of a new inbox item.
 */
export type ApiScope = "run" | "workItem" | "inbox" | "newInbox";

interface ApiOp {
  scope: ApiScope;
  /** The role of the route; user when absent. */
  role?: Role;
  handle(c: ApiCall): unknown;
}

export type WorkApiOp =
  | "getRun"
  | "listRunMessages"
  | "getAgentPins"
  | "listRunInputs"
  | "getRunTimeline"
  | "listRunSessions"
  | "getRunDiagnostics"
  | "listRunChildren"
  | "getRunStartCommits"
  | "appendRunLog"
  | "getWorkItem"
  | "getWorkItemTimeline"
  | "listWorkItemRuns"
  | "listWorkItemInbox"
  | "mcpPatchWorkItem"
  | "mcpLinkPullRequest"
  | "workItemMessage"
  | "workItemCancel"
  | "workItemComplete"
  | "workItemStage"
  | "getInbox"
  | "resolveInbox"
  | "unresolveInbox"
  | "createInboxItem";

/** The operation of a name, or undefined when WorkDO does not serve it. */
export function apiOp(op: string): ApiOp | undefined {
  return Object.hasOwn(WORK_API, op) ? WORK_API[op as WorkApiOp] : undefined;
}

/** The factory of the object of a call; "" when the object does not exist. */
export function scopeFactory(repo: Repo, scope: ApiScope, params: Record<string, string>, body: unknown): string {
  switch (scope) {
    case "inbox":
      return repo.getInbox(params.id ?? "")?.factory_id ?? "";
    case "newInbox": {
      const b = (body ?? {}) as { factory_id?: string; work_item_id?: string };
      return b.factory_id ?? repo.getWorkItem(b.work_item_id)?.factory_id ?? "";
    }
    case "workItem":
      return params.id ? (repo.getWorkItem(params.id)?.factory_id ?? "") : "";
    case "run":
      return repo.getRun(params.runId ?? params.id ?? "")?.factory_id ?? "";
  }
}

const WORK_API: Record<WorkApiOp, ApiOp> = {
  // ---------- runs ----------
  getRun: { scope: "run", handle: (c) => c.repo.mustRun(c.runId) },
  listRunMessages: {
    scope: "run",
    handle: ({ repo, runId, params }) => {
      repo.mustRun(runId);
      // compacted_upto: messages at or below it are in R2 (runs/<id>/messages.ndjson).
      const upto = Number(repo.getRunState(runId, "compacted_upto") ?? "0");
      return {
        messages: repo.listMessages(runId, Number(params.after ?? 0), Number(params.limit ?? 0)),
        ...(upto > 0 ? { compacted_upto: upto } : {}),
      };
    },
  },
  listRunInputs: {
    scope: "run",
    handle: ({ repo, runId }) => {
      repo.mustRun(runId);
      return { inputs: repo.listInputs(runId) };
    },
  },
  getRunTimeline: {
    scope: "run",
    handle: ({ repo, runId }) => {
      repo.mustRun(runId);
      return { events: repo.listTimeline(runId) };
    },
  },
  listRunSessions: {
    scope: "run",
    handle: ({ repo, runId }) => {
      repo.mustRun(runId);
      return { sessions: sessionViews(repo.listSessions(runId), repo.snapshotPartsBySession(runId)) };
    },
  },
  getRunDiagnostics: {
    scope: "run",
    handle: ({ repo, runId }) => {
      repo.mustRun(runId);
      return { diagnostics: repo.listDiagnostics(runId) };
    },
  },
  listRunChildren: { scope: "run", handle: ({ repo, runId }) => ({ runs: repo.listRuns({ parentRunId: runId }) }) },
  getRunStartCommits: {
    scope: "run",
    handle: ({ repo, runId }) => {
      repo.mustRun(runId);
      const raw = repo.getRunState(runId, "start_commits");
      return { start_commits: raw ? (JSON.parse(raw) as unknown[]) : [] };
    },
  },
  appendRunLog: {
    scope: "run",
    handle: ({ txFx, runId, body }) =>
      txFx((fx) => {
        const r = fx.repo.mustRun(runId);
        const b = (body ?? {}) as { content?: string; data?: unknown };
        const m = fx.repo.appendMessage({
          id: newRoutedId("msg", fx.token),
          run_id: r.id,
          role: "system",
          content: b.content ?? "",
          ...(b.data !== undefined ? { data: b.data } : {}),
          created_at: iso(fx.now()),
        });
        fx.wakeRun(r.id);
        return { message_id: m.id };
      }),
  },

  // ---------- work items ----------
  getAgentPins: {
    scope: "workItem",
    handle: ({ repo, params }) => {
      const raw = repo.getMeta(`agent_pins:${params.id ?? ""}`);
      return { pins: raw ? (JSON.parse(raw) as Record<string, unknown>) : {} };
    },
  },
  getWorkItem: { scope: "workItem", handle: ({ repo, params }) => mustWorkItem(repo, params.id) },
  getWorkItemTimeline: {
    scope: "workItem",
    handle: ({ repo, params }) => ({ events: repo.listWorkItemEvents(mustWorkItem(repo, params.id).id) }),
  },
  listWorkItemRuns: {
    scope: "workItem",
    handle: ({ repo, params }) => ({ runs: repo.listRuns({ workItemId: mustWorkItem(repo, params.id).id }) }),
  },
  listWorkItemInbox: {
    scope: "workItem",
    handle: ({ repo, params }) => {
      const wi = mustWorkItem(repo, params.id);
      return { items: repo.listInbox({ resolved: false }).filter((i) => i.work_item_id === wi.id) };
    },
  },
  mcpPatchWorkItem: {
    scope: "workItem",
    handle: ({ txFx, params, body, meta }) =>
      txFx((fx) => {
        const wi = mustWorkItem(fx.repo, params.id);
        const b = (body ?? {}) as {
          local_owner?: string;
          labels?: Record<string, string>;
          event?: { kind: string; data?: Record<string, unknown>; run_id?: string; actor?: string };
        };
        if (b.local_owner !== undefined) wi.local_owner = b.local_owner;
        if (b.labels) wi.labels = { ...(wi.labels ?? {}), ...b.labels };
        if (b.local_owner !== undefined || b.labels) {
          wi.updated_at = iso(fx.now());
          fx.repo.putWorkItem(wi);
          projectWorkItem(fx, wi);
        }
        if (b.event?.kind)
          workItemEvent(
            fx,
            wi.id,
            b.event.kind,
            b.event.run_id ?? "",
            b.event.actor ?? actorOfMeta(meta),
            b.event.data ?? {},
          );
        return fx.repo.getWorkItem(wi.id);
      }),
  },
  mcpLinkPullRequest: {
    scope: "workItem",
    handle: ({ txFx, params, body }) =>
      txFx((fx) => {
        const wi = mustWorkItem(fx.repo, params.id);
        linkPullRequest(fx, wi, (body ?? {}) as PullRequestRef);
        return fx.repo.getWorkItem(wi.id);
      }),
  },
  workItemMessage: {
    scope: "workItem",
    handle: ({ txFx, params, body, meta }) =>
      txFx((fx) => {
        const wi = mustWorkItem(fx.repo, params.id);
        if (!wi.root_run_id) throw new OfError("operation_not_supported", `work item ${wi.id} has no foreman run`);
        const b = (body ?? {}) as { message?: string };
        if (!(b.message ?? "").trim()) throw invalid("message is required");
        const rec = sendMessage(fx, wi.root_run_id, b.message ?? "", actorOfMeta(meta));
        return { work_item_id: wi.id, run_id: wi.root_run_id, message_id: rec.message_id, outcome: rec.outcome };
      }),
  },
  workItemCancel: {
    scope: "workItem",
    handle: ({ txFx, params, body, meta }) =>
      txFx((fx) => {
        const wi = mustWorkItem(fx.repo, params.id);
        if (wi.stage === "cancelled" || wi.stage === "complete") throw conflict(`work item is already ${wi.stage}`);
        const actor = actorOfMeta(meta);
        const done = new Set<string>();
        for (const r of fx.repo.listRuns({ workItemId: wi.id })) {
          if (!r.parent_run_id) for (const id of cancel(fx, r.id, actor, true)) done.add(id);
        }
        const note = ((body ?? {}) as { reason?: string }).reason || "Cancelled via API";
        const cur = fx.repo.getWorkItem(wi.id)!;
        if (cur.stage !== "cancelled" && cur.stage !== "complete") setStage(fx, wi.id, "cancelled", actor, note);
        return { work_item: fx.repo.getWorkItem(wi.id), cancelled: [...done] };
      }),
  },
  workItemComplete: {
    scope: "workItem",
    handle: ({ txFx, params, body, meta }) =>
      txFx((fx) => {
        const wi = mustWorkItem(fx.repo, params.id);
        if (wi.stage === "cancelled" || wi.stage === "complete") throw conflict(`work item is already ${wi.stage}`);
        return completeWorkItem(fx, wi.id, actorOfMeta(meta), ((body ?? {}) as { note?: string }).note ?? "");
      }),
  },
  workItemStage: {
    scope: "workItem",
    role: "editor",
    handle: ({ txFx, params, body, meta }) =>
      txFx((fx) => {
        const b = (body ?? {}) as { stage?: string; reason?: string };
        if (!b.stage || !STAGES.includes(b.stage as Stage))
          throw invalid(`invalid stage ${JSON.stringify(b.stage ?? "")}`);
        // Engine.SetStage: the stage complete is a completion (write-back, inbox, notice).
        if (b.stage === "complete") return completeWorkItem(fx, params.id ?? "", actorOfMeta(meta), b.reason ?? "");
        // The stage cancelled with a root run cancels the root run tree, which cancels the work item.
        if (b.stage === "cancelled") {
          const wi = mustWorkItem(fx.repo, params.id ?? "");
          if (wi.stage === "cancelled") return wi;
          if (stageTerminal(wi.stage)) throw conflict(`work item is ${wi.stage}`);
          if (wi.root_run_id && fx.repo.getRun(wi.root_run_id)) {
            cancel(fx, wi.root_run_id, actorOfMeta(meta), true);
            return fx.repo.getWorkItem(wi.id)!;
          }
        }
        return setStage(fx, params.id ?? "", b.stage as Stage, actorOfMeta(meta), b.reason ?? "");
      }),
  },

  // ---------- inbox ----------
  getInbox: {
    scope: "inbox",
    handle: ({ repo, params }) => {
      const it = repo.getInbox(params.id ?? "");
      if (!it) throw notFound(`inbox item ${params.id}`);
      return it;
    },
  },
  resolveInbox: {
    scope: "inbox",
    handle: ({ repo, txFx, params, body, meta, env, teamId }) => {
      // ResolveInbox: an approval needs CanApprove (approvers.ts) first.
      const it = repo.getInbox(params.id ?? "");
      if (it?.kind !== "approval" || it.resolved) return txFx((fx) => resolveInbox(fx, params.id ?? "", body, meta));
      return canApprove(approverDeps(env, teamId), it.factory_id, meta.principal?.userId ?? "").then((ok) => {
        if (!ok) throw notAnApprover();
        return txFx((fx) => resolveInbox(fx, params.id ?? "", body, meta));
      });
    },
  },
  unresolveInbox: {
    scope: "inbox",
    handle: ({ txFx, params, body, meta }) =>
      txFx((fx) =>
        unresolveInbox(fx, params.id ?? "", actorOfMeta(meta), (body as { actor_label?: string } | null)?.actor_label),
      ),
  },
  createInboxItem: { scope: "newInbox", handle: ({ txFx, body }) => txFx((fx) => createInboxItem(fx, body)) },
};

function mustWorkItem(repo: Repo, id: string | undefined) {
  const wi = repo.getWorkItem(id);
  if (!wi) throw notFound(`work item ${id}`);
  return wi;
}

function actorOfMeta(meta: CallMeta): string {
  return meta.principal?.userId ?? meta.principal?.id ?? "api";
}

// ---------- inbox ----------

/**
 * A new inbox item of the REST API or of a Worker tool. A track_key returns
 * the item that the same key created before. A caller may choose the ID
 * (propose_definition_change names it in the version first).
 */
function createInboxItem(fx: Fx, body: unknown): InboxItem | undefined {
  const b = (body ?? {}) as Partial<InboxItem> & { track_key?: string };
  if (!b.kind || !b.title) throw invalid("kind and title are required");
  const key = b.track_key ? `inbox_track:${b.track_key}` : "";
  if (key) {
    const prev = fx.repo.getMeta(key);
    if (prev) return fx.repo.getInbox(prev);
  }
  const wi = b.work_item_id ? fx.repo.getWorkItem(b.work_item_id) : fx.repo.getWorkItem();
  let id = newRoutedId("inb", fx.token);
  if (b.id) {
    if (!b.id.startsWith("inb_") || partitionOf(b.id) !== fx.token)
      throw invalid(`inbox item ID ${b.id} does not belong to this work item`);
    if (fx.repo.getInbox(b.id)) return fx.repo.getInbox(b.id);
    id = b.id;
  }
  const it: InboxItem = {
    id,
    factory_id: b.factory_id ?? wi?.factory_id ?? "",
    ...(wi ? { work_item_id: wi.id } : {}),
    ...(b.run_id ? { run_id: b.run_id } : {}),
    kind: b.kind,
    title: b.title,
    body: b.body ?? "",
    ...(b.options ? { options: b.options } : {}),
    ...(b.questions ? { questions: b.questions } : {}),
    ...(b.assignee_id ? { assignee_id: b.assignee_id } : {}),
    ...(b.recipients ? { recipients: b.recipients } : wi?.creator_id ? { recipients: [wi.creator_id] } : {}),
    resolved: false,
    created_at: iso(fx.now()),
    version: 0,
  };
  fx.repo.createInbox(it);
  projectInbox(fx, it);
  if (key) fx.repo.setMeta(key, it.id);
  return it;
}

/** ResolveInbox of the REST API: the checks of the request, then answerInbox. */
function resolveInbox(fx: Fx, id: string, body: unknown, meta: CallMeta): unknown {
  const it = fx.repo.getInbox(id);
  if (!it) throw notFound(`inbox item ${id}`);
  const b = (body ?? {}) as { resolution?: string; message?: string; via?: string; actor_label?: string };
  const resolution = (b.resolution ?? "").trim();
  if (!resolution) throw invalid("resolution is required");
  return answerInbox(fx, it, resolution, actorOfMeta(meta), {
    ...(b.message ? { message: b.message } : {}),
    ...(b.via ? { via: b.via } : {}),
    ...(b.actor_label ? { label: b.actor_label } : {}),
  });
}

/** UnresolveWindow (engine/inbox.go). */
export const UNRESOLVE_WINDOW_MS = 10 * 60_000;

/**
 * UnresolveInbox: undoes a resolve within 10 minutes, while the run did not
 * consume the answer. The run gets a message that withdraws the answer.
 */
export function unresolveInbox(fx: Fx, id: string, actor: string, label?: string): InboxItem {
  const it = fx.repo.getInbox(id);
  if (!it) throw notFound(`inbox item ${id}`);
  if (!it.resolved) throw conflict("inbox item is not resolved");
  const at = it.resolved_at ? Date.parse(it.resolved_at) : NaN;
  if (!Number.isFinite(at) || fx.now() - at > UNRESOLVE_WINDOW_MS)
    throw conflict("the resolve is older than 10m0s and cannot be undone");
  if (it.resolved_via === "forge" || (!it.resolved_by && !it.resolved_via))
    throw conflict("the platform closed this item and it cannot be undone");
  if (it.run_id && it.answer_message_id) {
    const input = fx.repo.getInput(it.answer_message_id);
    if (input && input.state !== "pending") throw conflict("the run already read the answer");
  }
  const old = it.resolution ?? "";
  it.resolved = false;
  delete it.resolution;
  delete it.resolved_by;
  delete it.resolved_via;
  delete it.resolved_at;
  delete it.answer_message_id;
  fx.repo.putInbox(it);
  projectInbox(fx, it);
  if (it.work_item_id)
    workItemEvent(fx, it.work_item_id, "note", it.run_id ?? "", actor, { inbox_id: it.id, text: "resolve undone" });
  if (it.run_id && resumesRun(it)) {
    const short = [...old].length > 500 ? [...old].slice(0, 499).join("") + "…" : old;
    const msg = `[human correction] ${actorLabel(actor, label)} withdrew the answer to ${JSON.stringify(it.title)} (it was: ${short}). Ignore that answer. A new answer will arrive as a follow-up message.`;
    try {
      sendMessage(fx, it.run_id, msg, actor);
    } catch (e) {
      console.warn("work: deliver inbox unresolve", it.id, e);
    }
  }
  return it;
}

// ---------- run views ----------

/**
 * SessionView (api/runs_ext.go): the sessions of a run with the snapshot
 * parts that each one saved (parts: Repo.snapshotPartsBySession, one query).
 */
export function sessionViews(sessions: RunSession[], parts: Map<number, Set<string>>): Record<string, unknown>[] {
  return sessions.map((s) => sessionView(s, parts.get(s.session) ?? new Set()));
}

function sessionView(s: RunSession, parts: Set<string>): Record<string, unknown> {
  const u = s.usage ?? { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 };
  return {
    session: s.session,
    number: s.session,
    state: s.state,
    assigned_at: s.assigned_at,
    ...(s.started_at ? { started_at: s.started_at } : {}),
    ...(s.ready_at ? { ready_at: s.ready_at } : {}),
    ...(s.ended_at ? { ended_at: s.ended_at } : {}),
    ...(s.end_reason ? { end_reason: s.end_reason } : {}),
    ...(s.failure_code ? { failure_code: s.failure_code } : {}),
    ...(s.sandbox_reason ? { sandbox_reason: s.sandbox_reason } : {}),
    usage: { ...u, coverage: usageCoverage(u) },
    snapshot: { workspace: parts.has("workspace"), harness_state: parts.has("harness_state") },
  };
}
