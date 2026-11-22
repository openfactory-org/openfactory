// Port of internal/engine/inbox_test.go and the inbox part of
// integration_gaps_test.go: recipients and questions of inbox items, the
// blocked item of a run that waits for input, the follow-up text of a resolve
// (names, never raw user IDs; bookkeeping kinds send nothing), undo limits,
// and the outbound notice of an approval. Handoffs closed by a forge review or
// merge are in src/do/ingress/integration-gaps.test.ts (they need intake).
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, newRoutedId, newToken } from "@openfactory/core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { CallMeta } from "../../contracts/do-messages";
import type { AgentConfig, InboxItem, Run } from "../../engine/types";
import { call, bootstrap } from "../../../test/helpers";
import { FakeRunner, installFakeSandbox, pump, workStub, type FakeProvider } from "../../../test/fakes/runner";
import type { WorkDO } from "./index";
import { canApprove, setApproverDeps, teamApproverDeps, type ApproverDeps } from "./approvers";
import { isApprover } from "../../engine/principal";

const TEAM = "default";

const foreman: AgentConfig = {
  id: "agt_ib",
  factory_id: "fac_ib",
  name: "foreman",
  description: "",
  type: "FOREMAN",
  instructions: "lead",
  harness: { type: "claude", model: "m" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
};

const userMeta = (userId: string): CallMeta => ({
  requestId: "t",
  principal: { kind: "user", id: userId, userId, teamId: TEAM, role: "admin", factoryRoles: {} },
});
const meta: CallMeta = { requestId: "t" };

let provider: FakeProvider;
let uninstall: () => void;
let restoreApprovers: () => void;
beforeEach(() => {
  ({ provider, uninstall } = installFakeSandbox());
  // The factories of these tests are not in a TeamDO: no approvers list.
  restoreApprovers = setApproverDeps(() => ({ approvers: async () => [], user: async () => undefined }));
});
afterEach(() => {
  uninstall();
  restoreApprovers();
});

/** A root run of a work item created by usr_dana. */
async function creatorRun(token: string): Promise<{ w: WorkDO; run: Run; wiId: string }> {
  const w = workStub(TEAM, token);
  const out = await w.runsCreate(meta, {
    dispatch: {
      factoryId: "fac_ib",
      config: foreman,
      prompt: "work",
      actor: "usr_dana",
      title: "Work",
      creatorId: "usr_dana",
      workItemId: newRoutedId("wi", token),
    },
  });
  return { w, run: out.run as Run, wiId: out.workItemId! };
}

async function tool(w: WorkDO, runId: string, name: string, input: Record<string, unknown>) {
  const res = await w.toolCall({ runId, tool: name, input, key: "", turnId: "" });
  expect(res.isError, res.content[0]?.text).toBeFalsy();
  return (JSON.parse(res.content[0]!.text) as { inbox_id: string }).inbox_id;
}

const getInbox = (w: WorkDO, id: string) => w.api(meta, "getInbox", { id }, null) as Promise<InboxItem>;
const inputs = async (w: WorkDO, runId: string) =>
  ((await w.api(meta, "listRunInputs", { runId }, null)) as { inputs: { content: string; message_id: string }[] })
    .inputs;
const createItem = (w: WorkDO, body: Partial<InboxItem>) =>
  w.api(meta, "createInboxItem", {}, { title: "Title", body: "Body", ...body }) as Promise<InboxItem>;
const resolve = (w: WorkDO, m: CallMeta, id: string, resolution: string, label?: string) =>
  w.api(m, "resolveInbox", { id }, { resolution, ...(label ? { actor_label: label } : {}) }) as Promise<InboxItem>;
const unresolve = (w: WorkDO, m: CallMeta, id: string) =>
  w.api(m, "unresolveInbox", { id }, null) as Promise<InboxItem>;

describe("inbox", () => {
  it("recipients and questions; an answered item sends nothing; one open item per person", async () => {
    const token = newToken("w");
    const { w, run } = await creatorRun(token);
    const id = await tool(w, run.id, "ask_human", {
      question: "Pick",
      details: "Pick some",
      options: ["a", "b"],
      multi_select: true,
      recommended_option: "b",
    });
    const it = await getInbox(w, id);
    expect(it.recipients).toEqual(["usr_dana"]);
    expect(it.questions).toEqual([
      { question_id: "q1", text: "Pick", type: "multi_select", options: ["a", "b"], recommended_option: "b" },
    ]);
    // Options give one single_select question; a recommendation outside them is dropped.
    const it2 = await getInbox(
      w,
      await tool(w, run.id, "ask_human", { question: "Which?", options: ["x"], recommended_option: "z" }),
    );
    expect(it2.questions![0]).toEqual({ question_id: "q1", text: "Which?", type: "single_select", options: ["x"] });
    // Without options: a text question.
    const it3 = await getInbox(w, await tool(w, run.id, "ask_human", { question: "Why?" }));
    expect(it3.questions![0]!.type).toBe("text");
    // An approval without options offers approve and reject.
    const ap = await getInbox(w, await tool(w, run.id, "request_approval", { title: "Spec" }));
    expect(ap.options).toEqual(["approve", "reject"]);

    // An answered item is information only: a resolve sends nothing to the run.
    const info = await createItem(w, { kind: "answered", run_id: run.id });
    const before = (await inputs(w, run.id)).length;
    await resolve(w, userMeta("usr_dana"), info.id, "seen");
    expect(await inputs(w, run.id)).toHaveLength(before);
  });

  it("a handoff update creates a handoff item and moves the stage (TestPostUpdateHandoff)", async () => {
    const token = newToken("w");
    const { run, wiId } = await creatorRun(token);
    const out = await runInDurableObject(env.WORK.getByName(doName.work(TEAM, token)), async (inst, state) => {
      const w = inst as unknown as WorkDO;
      const call = (input: Record<string, unknown>) =>
        w.toolCall({ runId: run.id, tool: "post_update", input, key: "", turnId: "" });
      const a = await call({ text: "Triage done" });
      const b = await call({ text: "PR ready: https://github.com/acme/api/pull/42", kind: "handoff" });
      const empty = await call({ text: "" });
      const kinds = state.storage.sql
        .exec<{ payload: string }>("SELECT payload FROM outbox WHERE kind = 'notice.responder' ORDER BY rowid")
        .toArray()
        .map((r) => {
          const p = JSON.parse(r.payload) as { message?: { kind: string }; payload?: { message: { kind: string } } };
          return (p.message ?? p.payload?.message)!.kind;
        });
      return { a, b, empty, kinds };
    });
    expect(out.a.isError).toBeFalsy();
    expect(out.b.isError, out.b.content[0]?.text).toBeFalsy();
    expect(out.empty.isError).toBe(true);
    expect(out.kinds).toEqual(["update", "handoff"]);
    const w = workStub(TEAM, token);
    const wi = (await w.api(meta, "getWorkItem", { id: wiId }, null)) as { stage: string };
    expect(wi.stage).toBe("handoff");
    const items = (await w.api(meta, "listWorkItemInbox", { id: wiId }, null)) as { items: InboxItem[] };
    expect(items.items.map((i) => i.kind)).toEqual(["handoff"]);
    expect(items.items[0]).toMatchObject({ title: "Ready for your review", recipients: ["usr_dana"] });
  });

  it("question and approval bodies (TestHumanAndWorkItemTools)", async () => {
    const token = newToken("w");
    const { w, run } = await creatorRun(token);
    const q = await getInbox(w, await tool(w, run.id, "ask_human", { question: "Required?", context: "API field" }));
    expect(q.kind).toBe("question");
    expect(q.title).toBe("Required?");
    expect(q.body).toBe("Required?\n\nAPI field");
    // A long question: the title is cut to 100 bytes, the body keeps it whole.
    const long = "x".repeat(150);
    const lq = await getInbox(w, await tool(w, run.id, "ask_human", { question: long }));
    expect(lq.title).toBe("x".repeat(97) + "...");
    expect(lq.body).toBe(long);
    const ap = await getInbox(
      w,
      await tool(w, run.id, "request_approval", { title: "Spec", summary: "Approve", links: ["https://x/pull/1"] }),
    );
    expect(ap.kind).toBe("approval");
    expect(ap.body).toBe("Approve\n\nReview:\n- https://x/pull/1");
    const bad = await w.toolCall({ runId: run.id, tool: "ask_human", input: { question: " " }, key: "", turnId: "" });
    expect(bad.isError).toBe(true);
    expect(bad.content[0]!.text).toContain("question is required");
  });

  it("the outbound notice of an approval carries the inbox item, the factory and the options", async () => {
    const token = newToken("w");
    const { run } = await creatorRun(token);
    const rows = await runInDurableObject(env.WORK.getByName(doName.work(TEAM, token)), async (inst, state) => {
      const res = await (inst as unknown as WorkDO).toolCall({
        runId: run.id,
        tool: "request_approval",
        input: { title: "Spec approval", details: "ok?" },
        key: "",
        turnId: "",
      });
      const id = (JSON.parse(res.content[0]!.text) as { inbox_id: string }).inbox_id;
      const out = state.storage.sql
        .exec<{ payload: string }>("SELECT payload FROM outbox WHERE kind = 'notice.responder'")
        .toArray()
        .map(
          (r) =>
            JSON.parse(r.payload) as {
              message?: Record<string, unknown>;
              payload?: { message: Record<string, unknown> };
            },
        );
      return { id, out };
    });
    const msgs = rows.out.map((r) => (r.message ?? r.payload?.message)!).filter((m) => m.kind === "approval_request");
    expect(msgs).toHaveLength(1);
    expect(msgs[0]).toMatchObject({ inbox_id: rows.id, factory_id: "fac_ib", options: ["approve", "reject"] });
    expect(String(msgs[0]!.text)).toContain("**Approval needed: Spec approval**");
    await pump(TEAM, token);
  });

  it("a blocked root run creates one blocked item for the creator", async () => {
    const token = newToken("w");
    const { w, run } = await creatorRun(token);
    await pump(TEAM, token);
    const fr = new FakeRunner(provider.taskFor(run.id));
    const turnId = (await fr.ready()).data!.turn_id!;
    await fr.events([
      { type: "turn.started", turn_id: turnId },
      { type: "turn.ended", turn_id: turnId, data: { outcome: "needs_input", result: "Which branch?" } },
    ]);
    const r = await w.getRun(run.id);
    expect(r.state).toBe("blocked");
    const items = ((await w.api(meta, "listWorkItemInbox", { id: r.work_item_id! }, null)) as { items: InboxItem[] })
      .items;
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ kind: "blocked", body: "Which branch?", recipients: ["usr_dana"] });
    await pump(TEAM, token);
  });

  it("no blocked item while the person has an open item", async () => {
    const token = newToken("w");
    const { w, run } = await creatorRun(token);
    const q = await tool(w, run.id, "ask_human", { question: "Which DB?" });
    await pump(TEAM, token);
    const fr = new FakeRunner(provider.taskFor(run.id));
    const turnId = (await fr.ready()).data!.turn_id!;
    await fr.events([
      { type: "turn.started", turn_id: turnId },
      { type: "turn.ended", turn_id: turnId, data: { outcome: "needs_input", result: "waits" } },
    ]);
    const r = await w.getRun(run.id);
    const items = ((await w.api(meta, "listWorkItemInbox", { id: r.work_item_id! }, null)) as { items: InboxItem[] })
      .items;
    expect(items.map((i) => i.id)).toEqual([q]);
    await pump(TEAM, token);
  });

  it("unresolve: withdraws the answer once; not after the run read it or after the window", async () => {
    const token = newToken("w");
    const { w, run } = await creatorRun(token);
    const dana = userMeta("usr_dana");
    const id = await tool(w, run.id, "ask_human", { question: "Which DB?" });
    const got = await resolve(w, dana, id, "Postgres", "Dana Lee");
    expect(got.answer_message_id).toBeTruthy();
    const un = await unresolve(w, dana, id);
    expect(un.resolved).toBe(false);
    expect(un.resolution).toBeUndefined();
    expect((await inputs(w, run.id)).at(-1)!.content).toContain("[human correction] a team member withdrew the answer");
    expect(String(await unresolve(w, dana, id).catch((e: unknown) => e))).toMatch(/not resolved/);

    // Older than the window: no undo.
    const late = await tool(w, run.id, "ask_human", { question: "Late?" });
    await resolve(w, dana, late, "yes");
    await runInDurableObject(env.WORK.getByName(doName.work(TEAM, token)), (_i, state) => {
      state.storage.sql.exec(
        "UPDATE inbox_items SET data = json_set(data, '$.resolved_at', ?) WHERE id = ?",
        new Date(Date.now() - 11 * 60_000).toISOString(),
        late,
      );
    });
    expect(String(await unresolve(w, dana, late).catch((e: unknown) => e))).toMatch(/older than 10m0s/);

    // The run read the answer: no undo.
    await resolve(w, dana, id, "SQLite");
    await pump(TEAM, token);
    const fr = new FakeRunner(provider.taskFor(run.id));
    await fr.ready();
    expect(String(await unresolve(w, dana, id).catch((e: unknown) => e))).toMatch(/already read the answer/);
    await pump(TEAM, token);
  });

  describe("the follow-up of a resolve uses a name and skips bookkeeping", () => {
    const dana = "Dana Lee";
    const cases: {
      name: string;
      kind: InboxItem["kind"];
      m?: CallMeta;
      label?: string;
      options?: string[];
      want?: string;
    }[] = [
      { name: "question by user with name", kind: "question", label: dana, want: "[human answer] Dana Lee answered" },
      { name: "question by unknown user", kind: "question", want: "[human answer] a team member answered" },
      {
        name: "question by automation",
        kind: "question",
        m: {
          requestId: "t",
          principal: { kind: "service_account", id: "webhook:aut_1", teamId: TEAM, role: "admin", factoryRoles: {} },
        },
        want: "[human answer] an automation answered",
      },
      { name: "approval", kind: "approval", label: dana, want: "resolved by Dana Lee: ok" },
      { name: "blocked", kind: "blocked", label: dana, want: "[human response] Dana Lee responded" },
      { name: "handoff", kind: "handoff", label: dana },
      { name: "answered", kind: "answered", label: dana },
      { name: "failure", kind: "failure", label: dana },
      { name: "pull request review", kind: "review", label: dana },
      {
        name: "definition change review",
        kind: "review",
        label: dana,
        options: ["approve", "reject"],
        want: "[human response] Dana Lee responded",
      },
    ];
    for (const tc of cases) {
      it(tc.name, async () => {
        const token = newToken("w");
        const { w, run } = await creatorRun(token);
        const it = await createItem(w, {
          kind: tc.kind,
          run_id: run.id,
          ...(tc.options ? { options: tc.options } : {}),
        });
        const m = tc.m ?? userMeta("usr_dana");
        const before = (await inputs(w, run.id)).length;
        await resolve(w, m, it.id, "ok", tc.label);
        const after = await inputs(w, run.id);
        if (!tc.want) {
          expect(after).toHaveLength(before);
          // Undo sends no correction either: nothing was sent.
          await unresolve(w, m, it.id);
          expect(await inputs(w, run.id)).toHaveLength(before);
          return;
        }
        expect(after).toHaveLength(before + 1);
        const got = after.at(-1)!.content;
        expect(got).toContain(tc.want);
        expect(got).not.toContain("usr_");
      });
    }
  });

  it("the REST resolve names the caller by name or email", async () => {
    const { key } = await bootstrap();
    const token = newToken("w");
    const { w, run } = await creatorRun(token);
    const id = await tool(w, run.id, "ask_human", { question: "Which DB?" });
    const res = await call("POST", `/api/v1/inbox/${id}/resolve`, {
      key,
      body: { resolution: "Postgres", actor_label: "Spoofed Name" },
    });
    expect(res.status, JSON.stringify(res.body)).toBe(200);
    const last = (await inputs(w, run.id)).at(-1)!.content;
    expect(last).toContain("[human answer] admin@example.com answered");
    expect(last).not.toContain("Spoofed");
    expect(last).not.toContain("usr_");
  });
});

// Port of internal/engine/approvers_test.go.
describe("approvers", () => {
  const users: Record<string, { id: string; email: string; kind?: string; name?: string }> = {
    usr_ada: { id: "usr_ada", email: "ada@example.test" },
    usr_bob: { id: "usr_bob", email: "bob@example.test" },
  };
  const deps = (list: string[]): ApproverDeps => ({
    approvers: () => Promise.resolve(list),
    user: (id) => Promise.resolve(users[id]),
  });

  it("only a listed approver resolves an approval; any member answers a question (TestApproversList)", async () => {
    const restore = setApproverDeps(() => deps(["Ada@Example.test"]));
    try {
      const token = newToken("w");
      const { w, run } = await creatorRun(token);
      const id = await tool(w, run.id, "request_approval", { title: "Spec", summary: "Review the spec" });
      // A member who is not in the list cannot approve, even an admin.
      expect(String(await resolve(w, userMeta("usr_bob"), id, "approved").catch((e: unknown) => e))).toMatch(
        /approvers listed in factory.yaml/,
      );
      // A sender without a linked account cannot approve.
      expect(String(await resolve(w, meta, id, "approved").catch((e: unknown) => e))).toMatch(/not_authorized/);
      expect((await getInbox(w, id)).resolved).toBe(false);
      // A listed approver can, from any tool. The tool is recorded.
      const done = (await w.api(
        userMeta("usr_ada"),
        "resolveInbox",
        { id },
        { resolution: "approved", via: "chat" },
      )) as InboxItem;
      // The actor of a person is "user:<id>", as Go recorded it.
      expect(done).toMatchObject({ resolved: true, resolved_by: "user:usr_ada", resolved_via: "chat" });
      // The list applies to approvals only.
      const q = await tool(w, run.id, "ask_human", { question: "Required?" });
      expect((await resolve(w, userMeta("usr_bob"), q, "No")).resolved).toBe(true);
    } finally {
      restore();
    }
  });

  it("a factory that does not exist is an error (LoadFactoryRuntime)", async () => {
    const err = await teamApproverDeps(env as never, TEAM)
      .approvers("fac_missing")
      .catch((e: unknown) => e);
    expect(String(err)).toMatch(/not found/);
  });

  it("without a list every caller can (TestCanApproveWithoutList)", async () => {
    expect(await canApprove(deps([]), "fac", "")).toBe(true);
    expect(await canApprove(deps(["a@example.test"]), "fac", "")).toBe(false);
    expect(await canApprove(deps(["a@example.test"]), "fac", "usr_missing")).toBe(false);
    expect(isApprover(["a@example.test"], undefined)).toBe(false);
    expect(isApprover(["a@example.test"], { id: "u" })).toBe(false);
    expect(isApprover(["ci-bot"], { id: "u", kind: "service_account", name: "ci-bot" })).toBe(true);
  });
});
