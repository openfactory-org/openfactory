// The WorkDO side of event intake: one action applied in
// the caller's transaction, deduplicated by the dispatch ledger
// (actionKey, target). Port of engine/events.go routeToWorkItem,
// handlePullRequestClosed, deliverToRoot, startSideRun, dispatchMention and
// engine/inbox.go closeHandoffs. The merge completion (engine/mergecomplete.go)
// is in lifecycle.ts, because the root run's turn end triggers it.
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
import { buildDispatchPrompt, truncate } from "../../engine/text";
import {
  stageTerminal,
  type InboxItem,
  type PullRequestRef,
  type WorkItem,
  type WorkItemRef,
} from "../../engine/types";
import { createRun, dispatchRoot, type DispatchPlan } from "./engine";
import { newMsgId, type Fx } from "./fx";
import { armMergeCompletion, completeWorkItem, promptContextKey, sendMessage } from "./lifecycle";
import { projectWorkItem, workItemEvent } from "./projections";
import { writeBackPullRequest } from "./writeback";
import { actorLabel, resolutionMessage, resolveInboxItem, resumesRun } from "./workitems";

// completeWorkItem lives in lifecycle.ts (a merge completes the work item
// after the root run's turn); callers import it from here with the other
// work item operations.
export { completeWorkItem };

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

/** storeEventBundle: the R2 reference of a message's event context, read when the message is delivered. */
export function storeEventContext(fx: Fx, messageKey: string, runId: string, ref: string): void {
  fx.repo.putEventContext(messageKey, runId, ref);
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
    const num = Number(ev.source?.number ?? 0);
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
        answer(fx, it.id, resolution, ev.actor?.user_id || actor, inboxVia(ev), actor);
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
        answer(fx, it.id, text, ev.actor?.user_id || actor, inboxVia(ev), actor);
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

/** ResolveInbox from a chat or forge reply. An item that is gone or already resolved is skipped. */
function answer(fx: Fx, inboxId: string, resolution: string, actor: string, via: string, label?: string): void {
  const it = fx.repo.getInbox(inboxId);
  if (!it || it.resolved) return;
  answerInbox(fx, it, resolution, actor, { via, ...(label !== undefined ? { label } : {}) });
}

// ---------- work item operations ----------
//
// One copy each of Engine.ResolveInbox, Engine.LinkPullRequest and
// Engine.CompleteWorkItem (in lifecycle.ts). The REST API, the Factory MCP, the run tools and
// event intake call them inside their own transaction.

export interface AnswerOptions {
  /** Text sent to the run after the resolution. */
  message?: string;
  /** The tool that resolved the item (chat, forge, ...). */
  via?: string;
  /** A readable name of the actor (actorLabel). */
  label?: string;
}

/**
 * answerInbox (Engine.ResolveInbox): resolves an open item, writes the
 * answer or approval_decided event and sends the resolution to the run that
 * waits on it. The caller checks the approvers of an approval.
 *
 * The resolution and its follow-up message commit together: when the run
 * cannot take the message (sendMessage throws, for example a conflict because
 * the run cannot continue), nothing is stored and the caller gets the error.
 * Go stored the resolution first and only logged a failed follow-up. This
 * keeps one transaction per request, because a caught error inside
 * transactionSync would keep the half-written message and input rows. The
 * case is narrow: cancelling a run already resolves its open items.
 */
export function answerInbox(
  fx: Fx,
  it: InboxItem,
  resolution: string,
  actor: string,
  opts: AnswerOptions = {},
): InboxItem {
  resolveInboxItem(fx, it, resolution, actor, opts.via);
  workItemEvent(fx, it.work_item_id, it.kind === "approval" ? "approval_decided" : "answer", it.run_id ?? "", actor, {
    inbox_id: it.id,
    resolution: truncate(resolution, 4000),
  });
  if (it.run_id && resumesRun(it)) {
    const text = opts.message ? `${resolution}\n\n${opts.message}` : resolution;
    const rec = sendMessage(fx, it.run_id, resolutionMessage(it, actorLabel(actor, opts.label), text), actor);
    it.answer_message_id = rec.message_id;
    fx.repo.putInbox(it);
  }
  return it;
}

export interface LinkResult {
  workItem: WorkItem;
  /** True when the pull request was new on the work item. */
  added: boolean;
  /** The reference key of the pull request, or "" without a repo and a number. */
  key: string;
}

/**
 * linkPullRequest (Engine.LinkPullRequest): attaches a pull request to a
 * work item, or updates the one that matches by URL or by repository (any
 * case) and number. A new pull request gets its reference key (when it has a
 * repo and a number), a pr_linked event and the tracker write-back.
 */
export function linkPullRequest(fx: Fx, wi: WorkItem, input: PullRequestRef): LinkResult {
  const pr: PullRequestRef = { ...input };
  if (!pr.url && (!pr.repo || !pr.number)) throw invalid("pull request url or repo+number is required");
  if (!pr.state) pr.state = "open";
  const list = wi.pull_requests ?? [];
  const i = list.findIndex(
    (p) =>
      (!!pr.url && p.url === pr.url) ||
      (!!pr.repo && !!pr.number && (p.repo ?? "").toLowerCase() === pr.repo.toLowerCase() && p.number === pr.number),
  );
  const added = i < 0;
  const key = pr.repo && pr.number ? sourceKey(pullRequestSource(pr)) : "";
  if (!added) {
    const merged = { ...list[i]! };
    if (pr.forge) merged.forge = pr.forge;
    if (pr.repo) merged.repo = pr.repo;
    if (pr.number) merged.number = pr.number;
    if (pr.url) merged.url = pr.url;
    if (pr.branch) merged.branch = pr.branch;
    merged.state = pr.state;
    list[i] = merged;
  } else {
    list.push(pr);
    const refs = wi.references ?? [];
    if (key && !refs.some((r) => r.key === key)) {
      const ref: WorkItemRef = {
        kind: "pull_request",
        key,
        source: pullRequestSource(pr),
        added_at: new Date(fx.now()).toISOString(),
      };
      wi.references = [...refs, ref];
    }
  }
  wi.pull_requests = list;
  wi.updated_at = new Date(fx.now()).toISOString();
  fx.repo.putWorkItem(wi);
  if (added) {
    workItemEvent(fx, wi.id, "pr_linked", "", "system", { ...pr });
    // Attach the pull request to the tracker issues.
    writeBackPullRequest(fx, wi, pr);
  }
  projectWorkItem(fx, wi);
  return { workItem: wi, added, key };
}

/** handlePullRequestClosed: record the PR; after the last merge the foreman gets a turn, then completion. */
function handlePullRequestClosed(fx: Fx, wi0: WorkItem, ev: CoreEvent, actor: string): void {
  const merged = isMergeEvent(ev);
  const src = ev.source;
  const pr: PullRequestRef = {
    forge: normalizeProvider(ev.provider).toUpperCase(),
    repo: src.repo ?? "",
    number: Number(src.number ?? 0),
    url: src.url ?? "",
    branch: "",
    state: merged ? "merged" : "closed",
  };
  const prKey = sourceKey(pullRequestSource(pr));
  const isSourcePR = !!wi0.source_key && wi0.source_key === prKey;
  const cur = isSourcePR ? wi0 : linkPullRequest(fx, wi0, pr).workItem;
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
