// The WorkDO side of event intake (03 §7.1 steps 7-9): one action applied in
// the caller's transaction, deduplicated by the dispatch ledger
// (actionKey, target). Port of engine/events.go routeToWorkItem,
// handlePullRequestClosed, deliverToRoot, startSideRun, dispatchMention,
// engine/inbox.go closeHandoffs and engine/mergecomplete.go.
import { invalid, pullRequestSource, sourceKey } from "@openfactory/core";
import type { CoreEvent } from "../../contracts/do-messages";
import {
  CONTINUE_EVENT,
  formatFollowUp,
  identifiedBy,
  inboxVia,
  isBareDecision,
  isCloseEvent,
  isMergeEvent,
  normalizeProvider,
  parseDecision,
  autoFollowUp,
  type MessageTrust,
} from "../../engine/intake";
import { buildDispatchPrompt } from "../../engine/text";
import {
  isTerminal,
  stageTerminal,
  waitsForInput,
  type PullRequestRef,
  type Run,
  type WorkItem,
} from "../../engine/types";
import {
  createRun,
  dispatchRoot,
  newMsgId,
  projectWorkItem,
  resolveInboxItem,
  sendMessage,
  setStage,
  workItemEvent,
  notify,
  type DispatchPlan,
  type Fx,
} from "./engine";

export type ActionKind = "continue" | "createRoot" | "sideRun" | "deliverToRoot" | "mention";

export interface ApplyActionInput {
  actionKey: string;
  target: string;
  kind: ActionKind;
  event: CoreEvent;
  /** The event context of the message this action sends (IngressDO built and stored it). */
  bundle?: { ref: string; messageId: string; promptSection: string };
  /** For createRoot, sideRun, deliverToRoot and mention: the resolved dispatch. */
  dispatchRequest?: DispatchPlan & { automationId?: string; prompt: string };
  /** For continue: the continuation kind, sender trust and factory alias. */
  continuation?: { kind: string; trust: MessageTrust; alias: string };
}

export interface ApplyActionResult {
  ok: true;
  served: boolean;
  /** The work item is complete or cancelled after the action. */
  terminal: boolean;
  workItemId?: string;
  rootRunId?: string;
  rootAgent?: string;
}

const actorOf = (ev: CoreEvent) => ev.actor?.handle || ev.actor?.external_id || "";

function result(fx: Fx, served: boolean): ApplyActionResult {
  const wi = fx.repo.getWorkItem();
  const root = wi?.root_run_id ? fx.repo.getRun(wi.root_run_id) : undefined;
  return {
    ok: true,
    served,
    terminal: !!wi && stageTerminal(wi.stage),
    ...(wi ? { workItemId: wi.id } : {}),
    ...(root ? { rootRunId: root.id, rootAgent: root.agent_name ?? "" } : {}),
  };
}

/** applyAction in one transaction. A recorded (actionKey, target) is served without work. */
export function applyAction(fx: Fx, input: ApplyActionInput): ApplyActionResult {
  if (input.actionKey && fx.repo.hasDispatch(input.actionKey, input.target) !== undefined) return result(fx, true);
  const rec = (ref: string) => {
    if (input.actionKey) fx.repo.recordDispatch(input.actionKey, input.target, ref);
  };
  const b = input.bundle;
  const section = b?.promptSection ?? "";
  // The run message gets a routed ID of this WorkDO; the bundle keeps its own
  // message ID (the directory of the event files that its prompt section names).
  const mid = b ? newMsgId(fx) : undefined;
  switch (input.kind) {
    case "createRoot":
    case "mention": {
      const plan = input.dispatchRequest;
      if (!plan) throw invalid("dispatchRequest is required");
      const out = dispatchRoot(fx, {
        ...plan,
        prompt: plan.prompt + section,
        ...(mid ? { messageId: mid } : {}),
        ...(input.actionKey ? { ledger: { actionKey: input.actionKey, target: input.target } } : {}),
      });
      if (b && mid) storeEventContext(fx, out.followUp ? mid : promptContextKey(out.run.id), out.run.id, b.ref);
      return result(fx, true);
    }
    case "deliverToRoot": {
      const plan = input.dispatchRequest;
      const wi = fx.repo.getWorkItem();
      if (!plan || !wi?.root_run_id) throw invalid("deliverToRoot needs a work item with a root run");
      if (
        (plan as { alreadyServed?: boolean }).alreadyServed ||
        (input.actionKey && fx.repo.hasDispatch(input.actionKey, `wi:${wi.id}`) !== undefined)
      ) {
        rec(wi.root_run_id);
        return result(fx, true);
      }
      sendMessage(
        fx,
        wi.root_run_id,
        buildDispatchPrompt({ prompt: plan.prompt, event: input.event }) + section,
        plan.triggeredBy || "system",
        [],
        mid,
      );
      if (b && mid) storeEventContext(fx, mid, wi.root_run_id, b.ref);
      rec(wi.root_run_id);
      if (input.actionKey) fx.repo.recordDispatch(input.actionKey, `wi:${wi.id}`, wi.root_run_id);
      return result(fx, true);
    }
    case "sideRun": {
      const plan = input.dispatchRequest;
      const wi = fx.repo.getWorkItem();
      if (!plan || !wi) throw invalid("sideRun needs a work item");
      const run = createRun(
        fx,
        {
          factoryId: wi.factory_id,
          workItemId: wi.id,
          config: plan.config,
          prompt: buildDispatchPrompt({ prompt: plan.prompt, event: input.event }) + section,
          triggeredBy: plan.triggeredBy || "api",
          ...(plan.creator ? { creator: plan.creator } : {}),
          ...(plan.runAs ? { runAs: plan.runAs } : {}),
          ...(plan.metadata ? { metadata: plan.metadata } : {}),
        },
        plan.actor,
      );
      if (b) storeEventContext(fx, promptContextKey(run.id), run.id, b.ref);
      rec(run.id);
      return result(fx, true);
    }
    case "continue":
      return routeToWorkItem(fx, input, rec);
  }
}

/** closeHandoffs: a review at the forge or a merge closes the handoff items. */
function closeHandoffs(fx: Fx, wi: WorkItem, ev: CoreEvent, actor: string): void {
  let resolution = isMergeEvent(ev) ? "pull request merged" : "review submitted";
  if (actor) resolution += " by " + actor;
  for (const it of fx.repo.listInbox({ resolved: false })) {
    if (it.kind !== "handoff" || it.work_item_id !== wi.id) continue;
    resolveInboxItem(fx, it, resolution, actor, "forge");
    workItemEvent(fx, wi.id, "note", it.run_id ?? "", actor, {
      inbox_id: it.id,
      text: "handoff closed: " + resolution,
    });
  }
}

/** The event_context key of the first prompt of a run (eventctx.PromptKVKey). */
export const promptContextKey = (runId: string): string => `prompt:${runId}`;

/** storeEventBundle: the R2 reference of a message's event context, read when the message is delivered. */
export function storeEventContext(fx: Fx, messageKey: string, runId: string, ref: string): void {
  fx.repo.sql.exec(
    "INSERT OR IGNORE INTO event_context (message_id, run_id, r2_key, bytes, created_ms) VALUES (?, ?, ?, 0, ?)",
    messageKey,
    runId,
    ref,
    fx.now(),
  );
}

/** followUpEvent: a message to the root run (a cancelled root gets nothing), with the event context. */
function followUp(fx: Fx, wi: WorkItem, text: string, actor: string, b?: ApplyActionInput["bundle"]): boolean {
  if (!wi.root_run_id) return false;
  const root = fx.repo.getRun(wi.root_run_id);
  if (!root || root.state === "cancelled") return false;
  const mid = b ? newMsgId(fx) : undefined;
  sendMessage(fx, root.id, text + (b?.promptSection ?? ""), actor, [], mid);
  if (b && mid) storeEventContext(fx, mid, root.id, b.ref);
  return true;
}

function routeToWorkItem(fx: Fx, input: ApplyActionInput, rec: (ref: string) => void): ApplyActionResult {
  const ev = input.event;
  const wi = fx.repo.getWorkItem();
  if (!wi || stageTerminal(wi.stage)) {
    rec("");
    return result(fx, false);
  }
  const c = input.continuation ?? {
    kind: "",
    trust: { trusted: false, canApprove: false, canAnswer: false, reason: "" },
    alias: "",
  };
  const actor = actorOf(ev);
  if (inboxVia(ev) === "forge" && (isMergeEvent(ev) || ev.type.toLowerCase().includes("review_submitted")))
    closeHandoffs(fx, wi, ev, actor);

  if (c.kind === CONTINUE_EVENT) {
    const num = Number((ev.source as { number?: number })?.number ?? 0);
    if (num !== 0 && (isMergeEvent(ev) || isCloseEvent(ev))) handlePullRequestClosed(fx, wi, ev, actor);
    else followUp(fx, wi, formatFollowUp(ev, { trusted: false, reason: "" }), "system", input.bundle);
    rec(wi.root_run_id ?? "");
    return result(fx, true);
  }

  const mt = c.trust;
  const check: Record<string, unknown> = {
    provider: ev.provider,
    external_id: ev.actor?.external_id ?? "",
    handle: ev.actor?.handle ?? "",
    user_id: ev.actor?.user_id ?? "",
    trust: ev.actor?.trust ?? "",
    identified_by: identifiedBy(ev),
    trusted: mt.trusted,
    can_approve: mt.canApprove,
    can_answer: mt.canAnswer,
    reason: mt.reason,
  };
  const text = (ev.text ?? "").trim();
  if (text && (mt.canApprove || mt.canAnswer)) {
    const open = fx.repo.listInbox({ resolved: false }).filter((i) => i.work_item_id === wi.id);
    if (mt.canApprove) {
      for (const it of open) {
        if (it.kind !== "approval") continue;
        const decision = parseDecision(text, c.alias);
        if (!decision) continue;
        const resolution = isBareDecision(text, c.alias) ? decision : `${decision}: ${text}`;
        workItemEvent(fx, wi.id, "sender_checked", it.run_id ?? "", actor, {
          ...check,
          result: "approval_" + decision,
          inbox_id: it.id,
        });
        answer(fx, it.id, resolution, ev.actor?.user_id || actor, inboxVia(ev));
        rec(it.id);
        return result(fx, true);
      }
    }
    if (mt.canAnswer) {
      const it = open.find((i) => i.kind === "question");
      if (it) {
        workItemEvent(fx, wi.id, "sender_checked", it.run_id ?? "", actor, {
          ...check,
          result: "answer",
          inbox_id: it.id,
        });
        answer(fx, it.id, text, ev.actor?.user_id || actor, inboxVia(ev));
        rec(it.id);
        return result(fx, true);
      }
    }
  }
  if (!autoFollowUp(ev)) return result(fx, true);
  workItemEvent(fx, wi.id, "sender_checked", wi.root_run_id ?? "", actor, {
    ...check,
    result: mt.trusted ? "delivered" : "delivered_untrusted",
  });
  followUp(fx, wi, formatFollowUp(ev, mt), actor || "system", input.bundle);
  rec(wi.root_run_id ?? "");
  return result(fx, true);
}

/** ResolveInbox from a chat or forge reply: resolution, timeline and the answer to the run. */
function answer(fx: Fx, inboxId: string, resolution: string, actor: string, via: string): void {
  const it = fx.repo.getInbox(inboxId);
  if (!it || it.resolved) return;
  resolveInboxItem(fx, it, resolution, actor, via);
  workItemEvent(fx, it.work_item_id, it.kind === "approval" ? "approval_decided" : "answer", it.run_id ?? "", actor, {
    inbox_id: it.id,
    resolution,
  });
  if (it.run_id) {
    const rec = sendMessage(fx, it.run_id, `Answer to "${it.title}": ${resolution}`, actor);
    it.answer_message_id = rec.message_id;
    fx.repo.putInbox(it);
  }
}

/** LinkPullRequest inside the transaction: the PR on the work item and its reference key. */
export function linkPullRequest(fx: Fx, wi: WorkItem, pr: PullRequestRef): WorkItem {
  const list = wi.pull_requests ?? [];
  const i = list.findIndex((x) => x.forge === pr.forge && x.repo === pr.repo && x.number === pr.number);
  if (i >= 0)
    list[i] = {
      ...list[i]!,
      ...Object.fromEntries(Object.entries(pr).filter(([, v]) => v !== "" && v !== undefined)),
    } as PullRequestRef;
  else list.push(pr);
  wi.pull_requests = list;
  const key = sourceKey(pullRequestSource(pr as never));
  if (key && !(wi.references ?? []).some((r) => r.key === key)) {
    wi.references = [
      ...(wi.references ?? []),
      { kind: "pull_request", key, added_at: new Date(fx.now()).toISOString() },
    ];
  }
  wi.updated_at = new Date(fx.now()).toISOString();
  fx.repo.putWorkItem(wi);
  projectWorkItem(fx, wi);
  return wi;
}

/** handlePullRequestClosed: record the PR; after the last merge the foreman gets a turn, then completion. */
function handlePullRequestClosed(fx: Fx, wi0: WorkItem, ev: CoreEvent, actor: string): void {
  const merged = isMergeEvent(ev);
  const src = ev.source as { repo?: string; number?: number; url?: string };
  const pr: PullRequestRef = {
    forge: normalizeProvider(ev.provider).toUpperCase(),
    repo: src.repo ?? "",
    number: Number(src.number ?? 0),
    url: src.url ?? "",
    branch: "",
    state: merged ? "merged" : "closed",
  };
  const prKey = sourceKey(pullRequestSource(pr as never));
  const isSourcePR = !!wi0.source_key && wi0.source_key === prKey;
  const cur = isSourcePR ? wi0 : linkPullRequest(fx, wi0, pr);
  const ref = pr.url || `${pr.repo}#${pr.number}`;
  const who = actor || "someone";
  if (merged) {
    let anyMerged = isSourcePR;
    let anyOpen = false;
    for (const p of cur.pull_requests ?? []) {
      if (p.state === "merged") anyMerged = true;
      else if (p.state === "open" || p.state === "draft" || p.state === "") anyOpen = true;
    }
    if (anyMerged && !anyOpen) {
      const note = `Pull request ${ref} was merged.`;
      const root = cur.root_run_id ? fx.repo.getRun(cur.root_run_id) : undefined;
      if (root && root.state !== "cancelled") {
        armMergeCompletion(fx, cur.id, root, actor, note);
        sendMessage(
          fx,
          root.id,
          `[pull request update] Pull request ${ref} was merged by ${who}. No linked pull request is open now. ` +
            "Post a last message or start follow-up work if it is needed, then complete the work item with complete_work_item. " +
            "If you do not complete it in this turn, the platform completes it after the turn.",
          "system",
        );
        return;
      }
      completeWorkItem(fx, cur.id, actor, note);
      return;
    }
    workItemEvent(fx, cur.id, "note", "", actor, {
      text: "pull request merged; other linked pull requests are still open",
      pr: ref,
    });
  }
  if (!cur.root_run_id) return;
  const verb = merged ? "merged (other linked pull requests are still open)" : "closed without merging";
  followUp(
    fx,
    cur,
    `[pull request update] Pull request ${ref} was ${verb} by ${who}. Decide the next step; do not complete the work item unless the requested change has landed.`,
    "system",
  );
}

interface PendingMergeCompletion {
  run_id: string;
  actor: string;
  note: string;
  turns: number;
  was_done?: boolean;
}

/** armMergeCompletion: the fallback completion for the turn that the merge message starts. */
export function armMergeCompletion(fx: Fx, wiId: string, root: Run, actor: string, note: string): void {
  const p: PendingMergeCompletion = {
    run_id: root.id,
    actor,
    note,
    turns: root.turns ?? 0,
    ...(isTerminal(root.state) ? { was_done: true } : {}),
  };
  if (!waitsForInput(root.state) && !isTerminal(root.state)) p.turns++;
  fx.repo.setMeta(`merge_complete:${wiId}`, JSON.stringify(p));
}

/** checkMergeCompletion: called in the root run's turn-end transition. */
export function checkMergeCompletion(fx: Fx, r: Run): void {
  if (r.parent_run_id || !r.work_item_id) return;
  const key = `merge_complete:${r.work_item_id}`;
  const raw = fx.repo.getMeta(key);
  if (!raw) return;
  let p: PendingMergeCompletion;
  try {
    p = JSON.parse(raw) as PendingMergeCompletion;
  } catch {
    return;
  }
  if (p.run_id !== r.id) return;
  const ended = (r.turns ?? 0) > p.turns || (isTerminal(r.state) && !p.was_done);
  if (!ended) return;
  fx.repo.setMeta(key, "");
  const wi = fx.repo.getWorkItem(r.work_item_id);
  if (!wi || stageTerminal(wi.stage)) return;
  completeWorkItem(
    fx,
    wi.id,
    p.actor,
    p.note + " The foreman did not complete the work item in its turn after the merge, so the platform completed it.",
  );
}

/** CompleteWorkItem (in one transaction): stage, child cancels, open inbox items, notice. */
export function completeWorkItem(fx: Fx, wiId: string, actor: string, note: string): void {
  const wi = setStage(fx, wiId, "complete", actor || "system", note);
  for (const it of fx.repo.listInbox({ resolved: false })) {
    if (it.work_item_id === wi.id) resolveInboxItem(fx, it, "work item completed", actor || "system");
  }
  if (note) notify(fx, wi, "update", note);
}
