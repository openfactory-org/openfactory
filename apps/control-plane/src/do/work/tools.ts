// Effects of the per-run factory MCP tools (internal/orchestration) inside
// WorkDO. The idempotency row commits in the same transaction as the tool's
// effect (C8, plan rule 3). Errors are not stored, so the model can retry.
import { forbidden, invalid, isOfError, newRoutedId, OfError, type SourceRef } from "@openfactory/core";
import { iso } from "../../engine/sessions";
import { STAGES, stageTerminal, type InboxItem, type Run, type Stage, type WorkItemRef } from "../../engine/types";
import { recipientsOf, truncate } from "../../engine/text";
import {
  notify,
  projectInbox,
  projectWorkItem,
  runAgents,
  sendMessage,
  setStage,
  workItemEvent,
  type ChildPlan,
  type Fx,
} from "./engine";

export interface ToolCall {
  runId: string;
  tool: string;
  input: Record<string, unknown>;
  /** orch:idem:<run>:<turn>:<tool>:<sha256(canonical input)>; "" when the run has no active turn. */
  key: string;
  turnId: string;
  /** run_agents: the children, resolved by the Worker from the TeamDO config snapshot. */
  plans?: ChildPlan[];
}

export interface ToolResult {
  content: { type: "text"; text: string }[];
  isError?: boolean;
  _meta?: Record<string, unknown>;
}

const text = (v: unknown): ToolResult => ({
  content: [{ type: "text", text: typeof v === "string" ? v : JSON.stringify(v) }],
});
const errorResult = (msg: string): ToolResult => ({ content: [{ type: "text", text: msg }], isError: true });

/** Tools without side effects run without an idempotency row. */
const READ_ONLY = new Set(["get_work_item", "list_agents"]);

/**
 * The rate limit of send_message and follow_up_agent for one sender and one
 * target (sendmessage.go). It stops a loop of messages between two agents.
 */
export const SEND_MESSAGE_LIMIT = 10;
export const SEND_MESSAGE_WINDOW_MS = 60_000;

export function runTool(fx: Fx, call: ToolCall): ToolResult {
  const caller = fx.repo.getRun(call.runId);
  if (!caller) return errorResult(`run ${call.runId} not found`);
  if (call.tool === "list_agents") return text({ active_runs: activeTargets(fx, caller) });
  if (caller.state === "cancelled" && !READ_ONLY.has(call.tool)) {
    return errorResult("this run was cancelled; stop working");
  }
  if (call.key && !READ_ONLY.has(call.tool)) {
    const stored = fx.repo.getToolCall(call.key);
    if (stored !== undefined) {
      const texts = JSON.parse(stored) as string[];
      return {
        content: [
          ...texts.map((t) => ({ type: "text" as const, text: t })),
          { type: "text", text: `{"duplicate": true}` },
        ],
        _meta: { duplicate: true },
      };
    }
  }
  let res: ToolResult;
  try {
    res = dispatchTool(fx, caller, call);
  } catch (e) {
    if (isOfError(e)) return errorResult(e.message);
    throw e;
  }
  if (call.key && !res.isError && !READ_ONLY.has(call.tool)) {
    fx.repo.putToolCall(call.key, call.runId, call.turnId, call.tool, JSON.stringify(res.content.map((c) => c.text)));
  }
  return res;
}

function s(v: unknown): string {
  return typeof v === "string" ? v : "";
}

/** activeState: a run that finished its turn waits for messages; a failed, error or cancelled run does not. */
function activeState(r: Run): boolean {
  return r.state !== "failed" && r.state !== "error" && r.state !== "cancelled";
}

/** relation: parent, child, sibling or work_item; "" when sender cannot send a message to target. */
export function relation(sender: Run, target: Run): string {
  if (target.id === sender.id) return "";
  if (sender.parent_run_id && target.id === sender.parent_run_id) return "parent";
  if (target.parent_run_id && target.parent_run_id === sender.id) return "child";
  if (!sender.work_item_id || target.work_item_id !== sender.work_item_id) return "";
  if (sender.parent_run_id && target.parent_run_id === sender.parent_run_id) return "sibling";
  return "work_item";
}

/** openWorkItem: the work item of a run is not terminal. A run with no work item is open. */
function openWorkItem(fx: Fx, r: Run): boolean {
  if (!r.work_item_id) return true;
  const wi = fx.repo.getWorkItem(r.work_item_id);
  return !!wi && !stageTerminal(wi.stage);
}

/** activeTargets: the active runs that sender can send a message to, by run ID. */
export function activeTargets(
  fx: Fx,
  sender: Run,
): { run_id: string; name?: string; agent?: string; state: string; relation: string }[] {
  let cands: Run[] = [];
  if (sender.work_item_id) {
    if (!openWorkItem(fx, sender)) return [];
    cands = fx.repo.listRuns({ workItemId: sender.work_item_id });
  } else {
    const p = sender.parent_run_id ? fx.repo.getRun(sender.parent_run_id) : undefined;
    if (p) cands.push(p);
    cands.push(...fx.repo.listRuns({ parentRunId: sender.id }));
  }
  const out = new Map<string, { run_id: string; name?: string; agent?: string; state: string; relation: string }>();
  for (const c of cands) {
    const rel = relation(sender, c);
    if (!rel || out.has(c.id) || !activeState(c)) continue;
    out.set(c.id, {
      run_id: c.id,
      ...(c.name ? { name: c.name } : {}),
      ...(c.agent_name ? { agent: c.agent_name } : {}),
      state: c.state,
      relation: rel,
    });
  }
  return [...out.values()].sort((a, b) => (a.run_id < b.run_id ? -1 : a.run_id > b.run_id ? 1 : 0));
}

/**
 * checkLimit: sender used up its messages to target in the window. The count
 * comes from the messages stored on the target, so a failed delivery (which
 * stored no message) does not count.
 */
function checkLimit(fx: Fx, sender: string, target: string): void {
  const actor = "run:" + sender;
  const cut = fx.now() - SEND_MESSAGE_WINDOW_MS;
  let n = 0;
  for (const m of fx.repo.listMessages(target)) {
    if (m.role === "user" && Date.parse(m.created_at) > cut && messageActor(m.data) === actor) n++;
  }
  if (n >= SEND_MESSAGE_LIMIT) {
    throw new OfError(
      "conflict",
      `rate limit: at most ${SEND_MESSAGE_LIMIT} messages to run ${JSON.stringify(target)} in 1m0s; wait for its answer`,
    );
  }
}

/** engine.MessageActor: the actor stored in the data of a run message. */
function messageActor(data: unknown): string {
  const a = (data as { actor?: unknown } | null | undefined)?.actor;
  return typeof a === "string" ? a : "";
}

/** isDescendant: run id descends from ancestor (local parents). */
function isDescendant(fx: Fx, id: string, ancestor: string): boolean {
  let cur = fx.repo.getRun(id);
  for (let i = 0; i < 32 && cur?.parent_run_id; i++) {
    if (cur.parent_run_id === ancestor) return true;
    cur = fx.repo.getRun(cur.parent_run_id);
  }
  return false;
}

function dispatchTool(fx: Fx, caller: Run, call: ToolCall): ToolResult {
  const actor = "run:" + caller.id;
  const input = call.input ?? {};
  switch (call.tool) {
    case "run_agents": {
      const runs = runAgents(fx, caller.id, call.plans ?? [], s(input.summary));
      return text({ runs: runs.map((r) => ({ run_id: r.id, name: r.name, agent: r.agent_name, state: r.state })) });
    }
    case "follow_up_agent": {
      const id = s(input.run_id).trim();
      if (!isDescendant(fx, id, caller.id)) throw invalid(`run ${JSON.stringify(id)} is not one of your child runs`);
      checkLimit(fx, caller.id, id);
      const rec = sendMessage(fx, id, s(input.message), actor);
      return text({ message_id: rec.message_id, outcome: rec.outcome });
    }
    case "send_message": {
      // The schema names the field text (sendmessage.go); message is accepted too.
      const msg = (s(input.text) || s(input.message)).trim();
      if (!msg) throw invalid("text is required");
      const id = s(input.run_id).trim();
      if (id === caller.id) throw invalid("you cannot send a message to your own run");
      const target = fx.repo.getRun(id);
      if (!target) throw new OfError("resource_not_found", `run ${JSON.stringify(id)} not found`);
      const rel = relation(caller, target);
      if (!rel) {
        throw forbidden(
          `run ${JSON.stringify(id)} is not in your work item; you can send messages only to your parent, your children and the other runs of your work item`,
        );
      }
      if (!activeState(target) || !openWorkItem(fx, target)) {
        throw new OfError("conflict", `run ${JSON.stringify(id)} is not active (state ${target.state})`);
      }
      checkLimit(fx, caller.id, target.id);
      const rec = sendMessage(fx, target.id, msg, actor);
      return text({ message_id: rec.message_id, outcome: rec.outcome, relation: rel });
    }
    case "ask_human":
    case "request_approval": {
      const kind = call.tool === "ask_human" ? "question" : "approval";
      const wi = caller.work_item_id ? fx.repo.getWorkItem(caller.work_item_id) : undefined;
      const title = s(input.question) || s(input.title) || s(input.summary);
      if (!title) throw invalid(kind === "question" ? "question is required" : "title is required");
      let options = Array.isArray(input.options) ? (input.options as unknown[]).map(String) : undefined;
      // RequestHuman: an approval without options offers approve and reject.
      if (kind === "approval" && !options?.length) options = ["approve", "reject"];
      const recommended = s(input.recommended_option).trim();
      const qtype = !options?.length ? "text" : input.multi_select === true ? "multi_select" : "single_select";
      const it: InboxItem = {
        id: newRoutedId("inb", fx.token),
        factory_id: caller.factory_id,
        ...(wi ? { work_item_id: wi.id } : {}),
        run_id: caller.id,
        kind,
        title: truncate(title, 500),
        body: s(input.details) || s(input.body),
        ...(options ? { options } : {}),
        questions: [
          {
            question_id: "q1",
            text: title,
            type: qtype,
            ...(options?.length ? { options } : {}),
            ...(recommended && options?.includes(recommended) ? { recommended_option: recommended } : {}),
          },
        ],
        ...(recipientsOf(wi) ? { recipients: recipientsOf(wi) } : {}),
        resolved: false,
        created_at: iso(fx.now()),
        version: 0,
      };
      fx.repo.createInbox(it);
      projectInbox(fx, it);
      if (wi) {
        workItemEvent(fx, wi.id, kind === "question" ? "question" : "approval_requested", caller.id, actor, {
          inbox_id: it.id,
          title,
        });
        // The notice to the work source (RequestHuman).
        const body = it.body.trim();
        const notice =
          kind === "approval"
            ? `**Approval needed: ${title}**\n\n${body}\n\nReply \`approve\` to continue, or reply with the changes you want.`
            : `**Question: ${title}**\n\n${body}\n\nReply here with your answer.`;
        notify(fx, wi, kind === "approval" ? "approval_request" : "question", notice, it.id, options);
        if (kind === "approval" && !stageTerminal(wi.stage)) setStage(fx, wi.id, "awaiting_approval", actor);
      }
      return text({ inbox_id: it.id, status: "waiting" });
    }
    case "post_update": {
      const wi = caller.work_item_id ? fx.repo.getWorkItem(caller.work_item_id) : undefined;
      if (!wi) throw invalid("the run has no work item");
      const kind = s(input.kind) || "update";
      workItemEvent(fx, wi.id, "message_out", caller.id, actor, { text: truncate(s(input.message), 4000), kind });
      notify(fx, wi, kind, s(input.message));
      return text({ posted: true });
    }
    case "set_stage": {
      if (!caller.work_item_id) throw invalid("the run has no work item");
      const stage = s(input.stage) as Stage;
      if (!STAGES.includes(stage) || stageTerminal(stage)) throw invalid(`invalid stage ${JSON.stringify(stage)}`);
      const wi = fx.repo.getWorkItem(caller.work_item_id);
      if (wi?.root_run_id !== caller.id && !caller.parent_run_id) throw forbidden("a side run cannot set the stage");
      const out = setStage(fx, caller.work_item_id, stage, actor, s(input.reason));
      return text({ stage: out.stage });
    }
    case "complete_work_item": {
      if (!caller.work_item_id) throw invalid("the run has no work item");
      const out = setStage(fx, caller.work_item_id, "complete", actor, s(input.summary));
      for (const it of fx.repo.listInbox({ resolved: false })) {
        it.resolved = true;
        it.resolution = "completed";
        it.resolved_by = actor;
        it.resolved_at = iso(fx.now());
        fx.repo.putInbox(it);
        projectInbox(fx, it);
      }
      notify(fx, out, "handoff", s(input.summary) || "The work is complete.");
      return text({ stage: out.stage });
    }
    case "link_pull_request":
    case "link_issue": {
      const wi = caller.work_item_id ? fx.repo.getWorkItem(caller.work_item_id) : undefined;
      if (!wi) throw invalid("the run has no work item");
      const key = s(input.key);
      if (!key) throw invalid("key is required");
      const kind = call.tool === "link_pull_request" ? "pull_request" : "issue";
      // link_issue: the Worker resolved the issue (linkissue.ts) into key and source.
      const source = input.source as SourceRef | undefined;
      const ref: WorkItemRef =
        kind === "issue" && source ? { kind, key, source, added_at: iso(fx.now()) } : { kind, key, ...input };
      const refs = wi.references ?? [];
      if (!refs.some((r) => r.key === key)) {
        wi.references = [...refs, ref];
        if (kind === "pull_request" && typeof input.number === "number") {
          wi.pull_requests = [
            ...(wi.pull_requests ?? []),
            {
              forge: s(input.forge),
              repo: s(input.repo),
              number: input.number,
              url: s(input.url),
              branch: s(input.branch),
              state: "open",
            },
          ];
        }
        wi.updated_at = iso(fx.now());
        fx.repo.putWorkItem(wi);
        fx.repo.sql.exec(
          "INSERT OR IGNORE INTO work_item_refs (key, kind, added_ms, data) VALUES (?, ?, ?, ?)",
          key,
          kind,
          fx.now(),
          JSON.stringify(ref),
        );
        workItemEvent(fx, wi.id, kind === "pull_request" ? "pr_linked" : "issue_linked", caller.id, actor, { key });
        projectWorkItem(fx, wi);
      }
      return text({ linked: key });
    }
    case "get_work_item": {
      const wi = caller.work_item_id ? fx.repo.getWorkItem(caller.work_item_id) : undefined;
      if (!wi) return text({ work_item: null });
      const children = fx.repo.listRuns({ workItemId: wi.id }).map((r) => ({
        run_id: r.id,
        name: r.name,
        agent: r.agent_name,
        state: r.state,
        parent: r.parent_run_id ?? "",
        result: truncate(r.result ?? "", 1500),
        error: truncate(r.error ?? "", 500),
      }));
      const open = fx.repo.listInbox({ resolved: false }).map((i) => ({ id: i.id, kind: i.kind, title: i.title }));
      return text({
        work_item: {
          id: wi.id,
          title: wi.title,
          stage: wi.stage,
          source: wi.source,
          pull_requests: wi.pull_requests ?? [],
        },
        runs: children,
        open_requests: open,
      });
    }
  }
  return errorResult(`unknown tool ${call.tool}`);
}
