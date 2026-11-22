// Effects of the per-run factory MCP tools (internal/orchestration) inside
// WorkDO. The idempotency row commits in the same transaction as the tool's
// effect (C8, plan rule 3). Errors are not stored, so the model can retry.
import { conflict, forbidden, invalid, isOfError, newRoutedId, notFound } from "@openfactory/core";
import { iso } from "../../engine/sessions";
import { isTerminal, STAGES, stageTerminal, type InboxItem, type Run, type Stage, type WorkItemRef } from "../../engine/types";
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

const text = (v: unknown): ToolResult => ({ content: [{ type: "text", text: typeof v === "string" ? v : JSON.stringify(v) }] });
const errorResult = (msg: string): ToolResult => ({ content: [{ type: "text", text: msg }], isError: true });

/** Tools without side effects run without an idempotency row. */
const READ_ONLY = new Set(["get_work_item", "list_agents"]);

export function runTool(fx: Fx, call: ToolCall): ToolResult {
  const caller = fx.repo.getRun(call.runId);
  if (!caller) return errorResult(`run ${call.runId} not found`);
  if (call.key && !READ_ONLY.has(call.tool)) {
    const stored = fx.repo.getToolCall(call.key);
    if (stored !== undefined) {
      const texts = JSON.parse(stored) as string[];
      return {
        content: [...texts.map((t) => ({ type: "text" as const, text: t })), { type: "text", text: `{"duplicate": true}` }],
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

function dispatchTool(fx: Fx, caller: Run, call: ToolCall): ToolResult {
  const actor = "run:" + caller.id;
  const input = call.input ?? {};
  switch (call.tool) {
    case "run_agents": {
      const runs = runAgents(fx, caller.id, call.plans ?? [], s(input.summary));
      return text({ runs: runs.map((r) => ({ run_id: r.id, name: r.name, agent: r.agent_name, state: r.state })) });
    }
    case "follow_up_agent": {
      const target = fx.repo.getRun(s(input.run_id));
      if (!target || target.parent_run_id !== caller.id) throw invalid(`run ${s(input.run_id)} is not a child of this run`);
      const rec = sendMessage(fx, target.id, s(input.message), actor);
      return text({ message_id: rec.message_id, outcome: rec.outcome });
    }
    case "send_message": {
      const target = fx.repo.getRun(s(input.run_id));
      if (!target) throw notFound(`run ${s(input.run_id)}`);
      if (target.id === caller.id) throw invalid("a run cannot send a message to itself");
      if (!caller.work_item_id || target.work_item_id !== caller.work_item_id) {
        throw forbidden("send_message reaches only runs of the same work item");
      }
      if (isTerminal(target.state) && target.state === "cancelled") throw conflict(`run ${target.id} is cancelled`);
      const rec = sendMessage(fx, target.id, s(input.message), actor);
      return text({ message_id: rec.message_id, outcome: rec.outcome });
    }
    case "ask_human":
    case "request_approval": {
      const kind = call.tool === "ask_human" ? "question" : "approval";
      const wi = caller.work_item_id ? fx.repo.getWorkItem(caller.work_item_id) : undefined;
      const title = s(input.question) || s(input.title) || s(input.summary);
      if (!title) throw invalid(kind === "question" ? "question is required" : "title is required");
      const options = Array.isArray(input.options) ? (input.options as unknown[]).map(String) : undefined;
      const it: InboxItem = {
        id: newRoutedId("inb", fx.token),
        factory_id: caller.factory_id,
        ...(wi ? { work_item_id: wi.id } : {}),
        run_id: caller.id,
        kind,
        title: truncate(title, 500),
        body: s(input.details) || s(input.body),
        ...(options ? { options } : {}),
        questions: [{ question_id: "q1", text: title, type: options?.length ? "single_select" : "text", ...(options ? { options } : {}) }],
        ...(recipientsOf(wi) ? { recipients: recipientsOf(wi) } : {}),
        resolved: false,
        created_at: iso(fx.now()),
        version: 0,
      };
      fx.repo.createInbox(it);
      projectInbox(fx, it);
      if (wi) {
        workItemEvent(fx, wi.id, kind === "question" ? "question" : "approval_requested", caller.id, actor, { inbox_id: it.id, title });
        notify(fx, wi, kind, title, it.id);
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
      const ref: WorkItemRef = { kind, key, ...input };
      const refs = wi.references ?? [];
      if (!refs.some((r) => r.key === key)) {
        wi.references = [...refs, ref];
        if (kind === "pull_request" && typeof input.number === "number") {
          wi.pull_requests = [
            ...(wi.pull_requests ?? []),
            { forge: s(input.forge), repo: s(input.repo), number: input.number, url: s(input.url), branch: s(input.branch), state: "open" },
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
      return text({ work_item: { id: wi.id, title: wi.title, stage: wi.stage, source: wi.source, pull_requests: wi.pull_requests ?? [] }, runs: children, open_requests: open });
    }
  }
  return errorResult(`unknown tool ${call.tool}`);
}
