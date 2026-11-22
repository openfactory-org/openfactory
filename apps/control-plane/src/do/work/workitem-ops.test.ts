// One copy of LinkPullRequest, CompleteWorkItem and ResolveInbox
// (internal/engine/workitems.go) behind every entry point: the run tools,
// the Factory MCP and REST ops of WorkDO, and event intake. Each entry point
// must match pull requests the same way, write one pr_linked event and one
// tracker write-back, and complete a work item with the same side effects.
import type { OutboundMessage } from "@openfactory/core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { CallMeta, CoreEvent, OutboxEnvelope } from "../../contracts/do-messages";
import { CONTINUE_EVENT } from "../../engine/intake";
import type { InboxItem, PullRequestRef, WorkItem } from "../../engine/types";
import { deliverNotice } from "../../notify/index";
import { buildRuns, settle, withFx, workStub } from "../../orchestration/fixture.test-util";
import { setNoticeDeliverer } from "./index";
import { applyAction } from "./intake";

const meta: CallMeta = { requestId: "t" };

let notices: OutboxEnvelope[] = [];
beforeEach(() => {
  notices = [];
  setNoticeDeliverer(async (_env, e) => {
    notices.push(e);
  });
});
afterEach(async () => {
  await settle();
  setNoticeDeliverer((ev, e) => deliverNotice(ev, e));
});

const messageOf = (e: OutboxEnvelope) => (e.payload as unknown as { message: OutboundMessage }).message;

async function delivered(token: string): Promise<OutboxEnvelope[]> {
  for (let i = 0; i < 3; i++) await workStub(token).pump();
  return notices;
}

/** A work item in building with a linked tracker issue, a root run (unless noRoot) and a child run. */
async function setup(noRoot = false) {
  const f = noRoot
    ? await buildRuns({ wi: { stage: "building" } }, {})
    : await buildRuns(
        { wi: { stage: "building", root: "root" } },
        {
          root: { type: "FOREMAN", workItem: "wi", state: "running" },
          child: { workItem: "wi", parent: "root", state: "running" },
        },
      );
  await withFx(f.token, (fx) => {
    const wi = fx.repo.getWorkItem(f.ids.wi!)!;
    wi.references = [
      {
        kind: "issue",
        key: "linear:ENG-1",
        source: { provider: "linear", kind: "issue", external_id: "ENG-1" },
        added_at: new Date(fx.now()).toISOString(),
      },
    ];
    fx.repo.putWorkItem(wi);
  });
  return f;
}

type Fixture = Awaited<ReturnType<typeof setup>>;

function prEvent(pr: { repo: string; number: number; url: string }, merged = false): CoreEvent {
  return {
    id: "evt_1",
    team_id: "default",
    provider: "github",
    type: merged ? "pull_request_merged" : "pull_request_closed",
    actor: { provider: "github", external_id: "1", handle: "octo", is_bot: false },
    source: { provider: "github", kind: "pull_request", ...pr },
    attributes: {},
    received_at: new Date().toISOString(),
  };
}

const intake = (f: Fixture, ev: CoreEvent) =>
  withFx(f.token, (fx) =>
    applyAction(fx, {
      actionKey: "",
      target: "",
      kind: "continue",
      event: ev,
      continuation: {
        kind: CONTINUE_EVENT,
        trust: { trusted: false, canApprove: false, canAnswer: false, reason: "" },
        alias: "",
      },
    }),
  );

const fullPR = (state: PullRequestRef["state"]): PullRequestRef => ({
  forge: "GITHUB",
  repo: "acme/api",
  number: 42,
  url: "https://github.com/acme/api/pull/42",
  branch: "feat",
  state,
});
/** The same pull request with another spelling of the repository and no URL. */
const samePR = (state: PullRequestRef["state"]): PullRequestRef => ({
  forge: "GITHUB",
  repo: "ACME/api",
  number: 42,
  url: "",
  branch: "",
  state,
});

interface LinkCase {
  name: string;
  noRoot?: boolean;
  link(f: Fixture, pr: PullRequestRef): Promise<void>;
  /** The state the second link leaves. */
  second: PullRequestRef["state"];
}

const linkCases: LinkCase[] = [
  {
    name: "run tool link_pull_request",
    second: "draft",
    async link(f, pr) {
      const res = await workStub(f.token).toolCall({
        runId: f.ids.root!,
        tool: "link_pull_request",
        input: { pull_request: pr },
        key: "",
        turnId: "",
      });
      expect(res.isError, res.content[0]?.text).toBeFalsy();
    },
  },
  {
    name: "Factory MCP mcpLinkPullRequest",
    second: "draft",
    async link(f, pr) {
      await workStub(f.token).api(meta, "mcpLinkPullRequest", { id: f.ids.wi! }, pr);
    },
  },
  {
    name: "intake of a closed pull request",
    noRoot: true,
    second: "closed",
    async link(f, pr) {
      await intake(f, prEvent(pr));
    },
  },
];

describe("linkPullRequest behind every entry point", () => {
  it.each(linkCases)("$name: one pull request, one pr_linked event, one write-back", async (c) => {
    const f = await setup(c.noRoot);
    await c.link(f, fullPR(c.noRoot ? "closed" : "open"));
    await c.link(f, samePR(c.second));
    const { wi, events } = await withFx(f.token, (fx) => ({
      wi: fx.repo.getWorkItem(f.ids.wi!)!,
      events: fx.repo.listWorkItemEvents(f.ids.wi!).filter((e) => e.kind === "pr_linked"),
    }));
    // Matched by repository (any case) and number: one entry, state of the last link.
    expect(wi.pull_requests).toHaveLength(1);
    expect(wi.pull_requests![0]).toMatchObject({
      repo: "ACME/api",
      number: 42,
      url: "https://github.com/acme/api/pull/42",
      state: c.second,
    });
    expect((wi.references ?? []).filter((r) => r.key === "github:acme/api#42")).toHaveLength(1);
    // The pull request at the top level of the event data (dashboard and metrics read it there).
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({
      actor: "system",
      data: { repo: "acme/api", number: 42, url: "https://github.com/acme/api/pull/42" },
    });
    const attach = (await delivered(f.token))
      .filter((e) => e.kind === "notice.tracker")
      .map(messageOf)
      .filter((m) => m.kind === "attach_pull_request");
    expect(attach).toHaveLength(1);
    expect(attach[0]).toMatchObject({ source: { external_id: "ENG-1" }, pull_request: { number: 42 } });
  });
});

interface CompleteCase {
  name: string;
  noRoot?: boolean;
  complete(f: Fixture): Promise<void>;
}

const completeCases: CompleteCase[] = [
  {
    name: "run tool complete_work_item",
    async complete(f) {
      const res = await workStub(f.token).toolCall({
        runId: f.ids.root!,
        tool: "complete_work_item",
        input: { note: "Done." },
        key: "",
        turnId: "",
      });
      expect(res.isError, res.content[0]?.text).toBeFalsy();
    },
  },
  {
    name: "REST workItemComplete",
    async complete(f) {
      await workStub(f.token).api(meta, "workItemComplete", { id: f.ids.wi! }, { note: "Done." });
    },
  },
  {
    name: "REST workItemStage complete",
    async complete(f) {
      await workStub(f.token).api(meta, "workItemStage", { id: f.ids.wi! }, { stage: "complete", reason: "Done." });
    },
  },
  {
    name: "intake of the last merge (no root run)",
    noRoot: true,
    async complete(f) {
      await intake(f, prEvent({ repo: "acme/api", number: 7, url: "https://github.com/acme/api/pull/7" }, true));
    },
  },
];

describe("completeWorkItem behind every entry point", () => {
  it.each(completeCases)("$name: stage, child runs, inbox, write-back and notice", async (c) => {
    const f = await setup(c.noRoot);
    const item = (await workStub(f.token).api(
      meta,
      "createInboxItem",
      {},
      { kind: "question", title: "Q", body: "B", work_item_id: f.ids.wi! },
    )) as InboxItem;
    await c.complete(f);
    const state = await withFx(f.token, (fx) => ({
      wi: fx.repo.getWorkItem(f.ids.wi!) as WorkItem,
      item: fx.repo.getInbox(item.id)!,
      root: f.ids.root ? fx.repo.getRun(f.ids.root)!.state : "",
      child: f.ids.child ? fx.repo.getRun(f.ids.child)!.state : "",
    }));
    expect(state.wi.stage).toBe("complete");
    expect(state.item).toMatchObject({ resolved: true, resolution: "work item completed" });
    if (!c.noRoot) {
      // The child runs stop; the root run winds down on its own.
      expect(state.child).toBe("cancelled");
      expect(state.root).not.toBe("cancelled");
    }
    const sent = await delivered(f.token);
    const done = sent
      .filter((e) => e.kind === "notice.tracker")
      .map(messageOf)
      .filter((m) => m.issue_state === "done");
    expect(done).toHaveLength(1);
    const notice = sent
      .filter((e) => e.kind === "notice.responder")
      .map(messageOf)
      .filter((m) => m.text.startsWith("This work item is complete."));
    expect(notice).toHaveLength(1);
    expect(notice[0]!.kind).toBe("update");
  });

  it("a completed work item stays complete; a second completion sends nothing", async () => {
    const f = await setup();
    await workStub(f.token).api(meta, "workItemStage", { id: f.ids.wi! }, { stage: "complete" });
    const first = (await delivered(f.token)).length;
    const res = await workStub(f.token).toolCall({
      runId: f.ids.root!,
      tool: "complete_work_item",
      input: {},
      key: "",
      turnId: "",
    });
    expect(res.isError, res.content[0]?.text).toBeFalsy();
    expect((await delivered(f.token)).length).toBe(first);
  });
});

describe("REST workItemStage cancelled", () => {
  it("cancels the root run tree and the work item (Engine.SetStage)", async () => {
    const f = await setup();
    const wi = (await workStub(f.token).api(
      meta,
      "workItemStage",
      { id: f.ids.wi! },
      { stage: "cancelled", reason: "No longer needed." },
    )) as WorkItem;
    expect(wi.stage).toBe("cancelled");
    await withFx(f.token, (fx) => {
      expect(fx.repo.getRun(f.ids.root!)!.state).toBe("cancelled");
      expect(fx.repo.getRun(f.ids.child!)!.state).toBe("cancelled");
    });
    const again = (await workStub(f.token).api(
      meta,
      "workItemStage",
      { id: f.ids.wi! },
      { stage: "cancelled" },
    )) as WorkItem;
    expect(again.stage).toBe("cancelled");
  });

  it("without a root run only sets the stage", async () => {
    const f = await setup(true);
    const wi = (await workStub(f.token).api(
      meta,
      "workItemStage",
      { id: f.ids.wi! },
      { stage: "cancelled" },
    )) as WorkItem;
    expect(wi.stage).toBe("cancelled");
  });
});

describe("answerInbox", () => {
  it("the timeline keeps at most 4000 bytes of the resolution", async () => {
    const f = await setup();
    const w = workStub(f.token);
    const item = (await w.api(
      meta,
      "createInboxItem",
      {},
      { kind: "handoff", title: "H", body: "B", work_item_id: f.ids.wi! },
    )) as InboxItem;
    await w.api(meta, "resolveInbox", { id: item.id }, { resolution: "x".repeat(5000) });
    const ev = await withFx(f.token, (fx) => fx.repo.listWorkItemEvents(f.ids.wi!).find((e) => e.kind === "answer"));
    const res = (ev?.data as { resolution: string }).resolution;
    expect(res.startsWith("x".repeat(4000))).toBe(true);
    expect(res).toContain("[truncated 1000 bytes]");
  });
});
