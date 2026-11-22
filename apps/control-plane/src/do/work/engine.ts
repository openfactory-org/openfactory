// The engine inside WorkDO: synchronous commit functions over the WorkDO
// tables. Every function runs inside the caller's transactionSync; effects
// leave through fx.enqueue (outbox), fx.setTimer and fx.wake*.
//
// This module holds the entry operations that create runs (createRun,
// dispatchRoot, runAgents) and re-exports the engine modules, so callers
// import the engine from one path:
//
//   fx.ts           the effect context, config and timer names
//   projections.ts  timeline, work item events, IndexDO and RouterDO projections
//   writeback.ts    notices to the source and the tracker write-back
//   sessions.ts     slot requests, session end and cancel
//   workitems.ts    work item creation, stages and inbox resolve
//   lifecycle.ts    transitions, delivery, turn-end reports, cancel and completion
//
// Ports internal/dispatch (transition, applyResult, deliver, send, cancel,
// lose, end) and internal/engine (createRun, Dispatch, RunAgents, cancel,
// lifecycle and turn-end reporting).
import { conflict, forbidden, invalid, newRoutedId } from "@openfactory/core";
import { runPrincipals } from "../../engine/principal";
import { iso } from "../../engine/sessions";
import { runInputSender, stageForAgent } from "../../engine/text";
import { taskIdOf } from "../../engine/timeline";
import { isTerminal, stageTerminal, ZERO_USAGE, type AgentConfig, type Run, type WorkItem } from "../../engine/types";
import { newMsgId, type Fx } from "./fx";
import { sendMessage } from "./lifecycle";
import { projectRun, projectWorkItem, workItemEvent } from "./projections";
import { requestSlot } from "./sessions";
import { createWorkItem, inferStage, isSideRun } from "./workitems";

export { DEFAULT_SANDBOX_IMAGE, newMsgId, timerName, type EngineConfig, type Fx } from "./fx";
export { projectInbox, projectRun, projectWorkItem, rootRunOf, timeline, workItemEvent } from "./projections";
export { notify, notifyTop, trackerIssues, trackerOp, writeBackComplete, writeBackPullRequest } from "./writeback";
export { cancelSession, endSession, enqueueProvider, requestSlot, stopSession } from "./sessions";
export {
  actorLabel,
  createWorkItem,
  inferStage,
  isSideRun,
  resolutionMessage,
  resolveInboxItem,
  resumesRun,
  setStage,
  type NewWorkItem,
} from "./workitems";
export {
  cancel,
  cancelOne,
  cancelTree,
  deliver,
  EVENT_FILES_REF,
  lifecycleTarget,
  loseCode,
  loseSession,
  scheduleDataUpkeep,
  send,
  sendMessage,
  SPEND_GATE_TTL_MS,
  spendStopsTurn,
  transition,
  updateRun,
  type TransitionResult,
} from "./lifecycle";
export { taskIdOf };

// ---------- create runs ----------

export interface NewRun {
  id?: string;
  factoryId: string;
  workItemId?: string;
  parentRunId?: string;
  config: AgentConfig;
  name?: string;
  prompt: string;
  metadata?: Record<string, string>;
  triggeredBy?: string;
  creator?: Run["creator"];
  runAs?: Run["run_as"];
  source?: Run["source"];
  sourceRef?: string;
}

/**
 * createRunWith: the run (queued), its first message, the first queue entry
 * (dedupe key "prompt") and the slot request, in one transaction.
 */
export function createRun(
  fx: Fx,
  nr: NewRun,
  actor: string,
  msgId?: string,
  ledger?: { actionKey: string; target: string },
): Run {
  if (!nr.prompt.trim()) throw invalid("prompt is required");
  const now = iso(fx.now());
  const pr = runPrincipals(nr.factoryId, nr.triggeredBy, nr.creator, nr.runAs);
  const r: Run = {
    id: nr.id ?? newRoutedId("run", fx.token),
    version: 0,
    team_id: fx.teamId,
    factory_id: nr.factoryId,
    ...(nr.workItemId ? { work_item_id: nr.workItemId } : {}),
    ...(nr.parentRunId ? { parent_run_id: nr.parentRunId } : {}),
    ...(nr.config.id ? { agent_id: nr.config.id } : {}),
    agent_name: nr.config.name,
    name: nr.name || nr.config.name,
    state: "queued",
    prompt: nr.prompt,
    config: nr.config,
    ...(nr.metadata ? { metadata: nr.metadata } : {}),
    consumed_seq: 0,
    usage: { ...ZERO_USAGE },
    triggered_by: nr.triggeredBy || "api",
    ...(nr.source ? { source: nr.source } : {}),
    ...(nr.sourceRef ? { source_ref: nr.sourceRef } : {}),
    ...(pr.creator ? { creator: pr.creator } : {}),
    run_as: pr.runAs,
    created_at: now,
  };
  fx.repo.createRun(r);
  const mid = msgId || newMsgId(fx);
  fx.repo.appendMessage({
    id: mid,
    run_id: r.id,
    role: "user",
    content: r.prompt,
    ...(actor ? { data: { actor } } : {}),
    created_at: now,
  });
  fx.repo.appendInput({
    message_id: mid,
    run_id: r.id,
    kind: "user",
    sender: runInputSender(actor),
    content: r.prompt,
    dedupe_key: "prompt",
  });
  if (ledger) fx.repo.recordDispatch(ledger.actionKey, ledger.target, r.id);
  workItemEvent(fx, r.work_item_id, "run_started", r.id, actor, {
    agent: r.agent_name ?? "",
    name: r.name ?? "",
    parent_run_id: r.parent_run_id ?? "",
    triggered_by: r.triggered_by ?? "",
  });
  projectRun(fx, r);
  requestSlot(fx, r);
  return r;
}

// ---------- dispatch ----------

export interface DispatchPlan {
  factoryId: string;
  config: AgentConfig;
  prompt: string;
  actor: string;
  title: string;
  source?: WorkItem["source"];
  sourceKey?: string;
  automationId?: string;
  creatorId?: string;
  metadata?: Record<string, string>;
  triggeredBy?: string;
  creator?: Run["creator"];
  runAs?: Run["run_as"];
  eventText?: string;
  /** Work item ID chosen by the caller (router reservation). */
  workItemId?: string;
  ledger?: { actionKey: string; target: string };
  /** Repositories at commits the root run starts from (benchmarks, judges). */
  startingRepoRefs?: unknown[];
  /** Agent pins of a trial; the caller applied them to config already. */
  agentPins?: Record<string, unknown>;
  /** Message ID of a follow-up to the root run (the event context names it). */
  messageId?: string;
}

/**
 * Engine.Dispatch inside the WorkDO of the work item: a follow-up to the
 * root run when the work item has one, else a new work item and root run
 * (all in one transaction).
 */
export function dispatchRoot(fx: Fx, p: DispatchPlan): { workItem: WorkItem; run: Run; followUp: boolean } {
  if (p.ledger) {
    const ref = fx.repo.hasDispatch(p.ledger.actionKey, p.ledger.target);
    if (ref) {
      const run = fx.repo.mustRun(ref);
      const wi = fx.repo.getWorkItem(run.work_item_id);
      if (wi) return { workItem: wi, run, followUp: false };
    }
  }
  let wi = fx.repo.getWorkItem(p.workItemId);
  if (wi && stageTerminal(wi.stage)) throw conflict(`work item ${wi.id} is ${wi.stage}`);
  if (wi?.root_run_id) {
    const root = fx.repo.getRun(wi.root_run_id);
    if (root && root.state !== "cancelled") {
      const msgId = p.messageId || newMsgId(fx);
      sendMessage(fx, root.id, p.prompt, p.actor, [], msgId);
      if (p.ledger) fx.repo.recordDispatch(p.ledger.actionKey, p.ledger.target, root.id);
      return { workItem: wi, run: fx.repo.mustRun(root.id), followUp: true };
    }
  }
  if (!wi) {
    wi = createWorkItem(
      fx,
      {
        ...(p.workItemId ? { id: p.workItemId } : {}),
        factoryId: p.factoryId,
        title: p.title,
        ...(p.source ? { source: p.source } : {}),
        ...(p.sourceKey ? { sourceKey: p.sourceKey } : {}),
        ...(p.automationId ? { automationId: p.automationId } : {}),
        ...(p.creatorId ? { creatorId: p.creatorId } : {}),
      },
      p.actor,
      p.eventText,
    );
  }
  const run = createRun(
    fx,
    {
      factoryId: p.factoryId,
      workItemId: wi.id,
      config: p.config,
      prompt: p.prompt,
      ...(p.metadata ? { metadata: p.metadata } : {}),
      triggeredBy: p.triggeredBy || "api",
      ...(p.creator ? { creator: p.creator } : {}),
      ...(p.runAs ? { runAs: p.runAs } : {}),
    },
    p.actor,
    undefined,
    p.ledger,
  );
  if (!wi.root_run_id) {
    wi.root_run_id = run.id;
    wi.updated_at = iso(fx.now());
    fx.repo.putWorkItem(wi);
    projectWorkItem(fx, wi);
  }
  return { workItem: wi, run, followUp: false };
}

// ---------- run_agents ----------

export interface ChildPlan {
  config: AgentConfig;
  name: string;
  prompt: string;
  metadata?: Record<string, string>;
}

export const MAX_RUN_AGENTS_BATCH = 20;

/** checkOrchestrationDepth: walks local parents. A root run has depth 0. */
export function checkOrchestrationDepth(fx: Fx, parent: Run): void {
  let depth = 0;
  let cur: Run | undefined = parent;
  while (cur?.parent_run_id) {
    depth++;
    if (depth >= fx.cfg.maxDepth) break;
    cur = fx.repo.getRun(cur.parent_run_id);
  }
  if (depth >= fx.cfg.maxDepth) {
    throw forbidden(
      `run ${parent.id} is at the orchestration depth limit and cannot start child runs (OF_MAX_ORCHESTRATION_DEPTH is ${fx.cfg.maxDepth}; a root run has depth 0)`,
    );
  }
}

/** RunAgents: children of a parent run in one transaction (the parent lock is the transaction). */
export function runAgents(fx: Fx, parentId: string, plans: ChildPlan[], summary = ""): Run[] {
  if (plans.length === 0) throw invalid("agent_run_configs must contain at least one child");
  if (plans.length > MAX_RUN_AGENTS_BATCH)
    throw invalid(`at most ${MAX_RUN_AGENTS_BATCH} children per run_agents call`);
  const parent = fx.repo.mustRun(parentId);
  if (isTerminal(parent.state)) throw conflict(`parent run is ${parent.state}`);
  checkOrchestrationDepth(fx, parent);
  if (parent.work_item_id) {
    const wi = fx.repo.getWorkItem(parent.work_item_id);
    if (wi && stageTerminal(wi.stage)) throw conflict(`work item is ${wi.stage}`);
  }
  const callerName = parent.agent_name || parent.config.name;
  const actor = "run:" + parent.id;
  const out: Run[] = [];
  let best = "";
  let bestRank = 0;
  for (const p of plans) {
    if (p.config.type === "FOREMAN" || !canSpawnCheck(p.config, callerName, parent.config.type)) {
      throw forbidden(
        `agent ${JSON.stringify(p.config.name)} may not be started by ${JSON.stringify(callerName)} (spawnableBy)`,
      );
    }
    const child = createRun(
      fx,
      {
        factoryId: parent.factory_id,
        ...(parent.work_item_id ? { workItemId: parent.work_item_id } : {}),
        parentRunId: parent.id,
        config: p.config,
        name: p.name,
        prompt: p.prompt,
        ...(p.metadata ? { metadata: p.metadata } : {}),
        triggeredBy: actor,
        ...(parent.creator ? { creator: parent.creator } : {}),
        ...(parent.run_as ? { runAs: parent.run_as } : {}),
      },
      actor,
    );
    out.push(child);
    const [st, rank] = stageForAgent(p.config.type);
    if (rank > bestRank) [best, bestRank] = [st, rank];
  }
  if (parent.work_item_id) {
    if (summary) workItemEvent(fx, parent.work_item_id, "note", parent.id, actor, { summary, children: out.length });
    if (best && !isSideRun(fx, parent)) inferStage(fx, parent.work_item_id, best, actor);
  }
  return out;
}

function canSpawnCheck(target: AgentConfig, callerName: string, callerType: string): boolean {
  if (target.type === "FOREMAN") return false;
  if (!target.spawnable_by) return callerType === "FOREMAN";
  return target.spawnable_by.includes(callerName);
}
