// send_message, its rate limit shared with follow_up_agent, and active_runs
// of list_agents (internal/orchestration/sendmessage_test.go).
import { env as workersEnv } from "cloudflare:workers";
import { afterEach, describe, expect, it } from "vitest";
import { SEND_MESSAGE_LIMIT, SEND_MESSAGE_WINDOW_MS } from "../do/work/tools";
import { buildRuns, resultText, settle, withFx } from "./fixture.test-util";
import { callTool, setAgentSource, setRedactor, plannerRedactor, teamAgents } from "./index";

const identity = async () => (s: string) => s;
const env = () => workersEnv;

afterEach(async () => {
  await settle();
  setAgentSource(teamAgents);
  setRedactor(plannerRedactor);
});

/** The fixture of sendmessage_test.go: one work item with a tree, a side run, a second work item, runs without one. */
async function fixture() {
  setRedactor(identity);
  return buildRuns(
    { wi_1: { stage: "building", root: "run_root" }, wi_2: { stage: "building", root: "run_other" } },
    {
      run_root: { agent: "foreman", type: "FOREMAN", workItem: "wi_1", state: "succeeded" },
      run_a: { agent: "implement", workItem: "wi_1", parent: "run_root", state: "running" },
      run_b: { agent: "review", workItem: "wi_1", parent: "run_root", state: "blocked" },
      run_side: { agent: "watch", workItem: "wi_1", state: "running" },
      run_dead: { workItem: "wi_1", parent: "run_root", state: "cancelled" },
      run_other: { agent: "triage", workItem: "wi_2", state: "running" },
      run_solo: { state: "succeeded" },
      run_kid: { parent: "run_solo", state: "running" },
      run_lone: { state: "running" },
    },
  );
}

type Fixture = Awaited<ReturnType<typeof fixture>>;

const send = (f: Fixture, from: string, to: string, text: string) =>
  callTool(env(), "default", f.ids[from]!, "send_message", { run_id: f.ids[to] ?? to, text });

/** The user messages of a run, with their actor. */
async function messages(f: Fixture, run: string): Promise<{ content: string; actor: string }[]> {
  return withFx(f.token, (fx) =>
    fx.repo
      .listMessages(f.ids[run]!)
      .filter((m) => m.role === "user")
      .map((m) => ({ content: m.content, actor: String((m.data as { actor?: string } | undefined)?.actor ?? "") })),
  );
}

/** Moves the messages of a run out of the rate-limit window. */
async function age(f: Fixture, run: string): Promise<void> {
  const old = new Date(Date.now() - SEND_MESSAGE_WINDOW_MS - 1000).toISOString();
  await withFx(f.token, (_fx, sql) => {
    sql.exec("UPDATE run_messages SET data = json_set(data, '$.created_at', ?) WHERE run_id = ?", old, f.ids[run]!);
  });
}

describe("send_message", () => {
  it("reaches the parent, children, siblings and runs of the work item", async () => {
    const f = await fixture();
    const cases: [string, string, string][] = [
      ["run_a", "run_root", "parent"],
      ["run_a", "run_b", "sibling"],
      ["run_root", "run_a", "child"],
      ["run_side", "run_root", "work_item"],
      ["run_b", "run_side", "work_item"],
      ["run_kid", "run_solo", "parent"],
      ["run_solo", "run_kid", "child"],
    ];
    for (const [from, to, rel] of cases) {
      const res = await send(f, from, to, `hello from ${from}`);
      expect(res.isError, `${from} -> ${to}: ${resultText(res)}`).toBeFalsy();
      expect(JSON.parse(resultText(res)).relation).toBe(rel);
      const last = (await messages(f, to)).at(-1)!;
      expect(last).toEqual({ content: `hello from ${from}`, actor: `run:${f.ids[from]}` });
    }
  });

  it("refuses runs outside the work item, inactive runs, its own run and unknown runs", async () => {
    const f = await fixture();
    const cases: [string, string, string][] = [
      ["run_a", "run_other", "not in your work item"],
      ["run_other", "run_a", "not in your work item"],
      ["run_a", "run_dead", "not active"],
      ["run_a", "run_a", "own run"],
      ["run_a", "run_nope", "not found"],
      ["run_lone", "run_solo", "not in your work item"],
    ];
    const before = await Promise.all(["run_other", "run_a", "run_dead", "run_solo"].map((r) => messages(f, r)));
    for (const [from, to, want] of cases) {
      const res = await send(f, from, to, "hi");
      expect(res.isError, `${from} -> ${to}`).toBe(true);
      expect(resultText(res)).toContain(want);
    }
    const after = await Promise.all(["run_other", "run_a", "run_dead", "run_solo"].map((r) => messages(f, r)));
    expect(after).toEqual(before);
  });

  it("refuses an empty text and a run of a finished work item", async () => {
    const f = await fixture();
    expect(resultText(await send(f, "run_a", "run_b", "  "))).toContain("text is required");
    await withFx(f.token, (fx) => {
      const wi = fx.repo.getWorkItem(f.ids.wi_1!)!;
      fx.repo.putWorkItem(Object.assign(wi, { stage: "complete" }));
    });
    const res = await send(f, "run_a", "run_b", "late");
    expect(res.isError).toBe(true);
    expect(resultText(res)).toContain("not active");
  });

  it("limits the messages of one sender to one target in the window", async () => {
    const f = await fixture();
    for (let i = 0; i < SEND_MESSAGE_LIMIT; i++) {
      const res = await send(f, "run_a", "run_b", `ping ${i}`);
      expect(res.isError, resultText(res)).toBeFalsy();
    }
    const over = await send(f, "run_a", "run_b", "one more");
    expect(over.isError).toBe(true);
    expect(resultText(over)).toContain("rate limit");
    // An other pair has its own count.
    expect((await send(f, "run_a", "run_root", "to parent")).isError).toBeFalsy();
    // After the window, the pair can send again.
    await age(f, "run_b");
    expect((await send(f, "run_a", "run_b", "later")).isError).toBeFalsy();
    const fromA = (await messages(f, "run_b")).filter((m) => m.actor === `run:${f.ids.run_a}`);
    expect(fromA).toHaveLength(SEND_MESSAGE_LIMIT + 1);
  });

  it("shares the limit with follow_up_agent", async () => {
    const f = await fixture();
    for (let i = 0; i < SEND_MESSAGE_LIMIT; i++) {
      expect((await send(f, "run_root", "run_a", `ping ${i}`)).isError).toBeFalsy();
    }
    expect((await send(f, "run_root", "run_a", "over")).isError).toBe(true);
    let extra = 0;
    for (let i = 0; i < 3; i++) {
      const res = await callTool(env(), "default", f.ids.run_root!, "follow_up_agent", {
        run_id: f.ids.run_a!,
        message: `more ${i}`,
      });
      if (!res.isError) extra++;
      else expect(resultText(res)).toContain("rate limit");
    }
    expect(extra).toBe(0);
  });

  it("follow_up_agent reaches descendants only", async () => {
    const f = await fixture();
    const ok = await callTool(env(), "default", f.ids.run_root!, "follow_up_agent", {
      run_id: f.ids.run_a!,
      message: "revise",
    });
    expect(ok.isError, resultText(ok)).toBeFalsy();
    const bad = await callTool(env(), "default", f.ids.run_a!, "follow_up_agent", {
      run_id: f.ids.run_b!,
      message: "x",
    });
    expect(bad.isError).toBe(true);
    expect(resultText(bad)).toContain("not one of your child runs");
  });

  it("a cancelled run cannot send", async () => {
    const f = await fixture();
    const res = await send(f, "run_dead", "run_root", "hi");
    expect(res.isError).toBe(true);
    expect(resultText(res)).toContain("cancelled");
  });
});

describe("list_agents", () => {
  it("shows the active runs that the caller can send a message to", async () => {
    const f = await fixture();
    setAgentSource(async () => []);
    const res = await callTool(env(), "default", f.ids.run_a!, "list_agents", {});
    expect(res.isError, resultText(res)).toBeFalsy();
    const out = JSON.parse(resultText(res)) as { active_runs: { run_id: string; relation: string }[] };
    const got = Object.fromEntries(out.active_runs.map((r) => [f.name(r.run_id), r.relation]));
    expect(got).toEqual({ run_root: "parent", run_b: "sibling", run_side: "work_item" });
  });
});
