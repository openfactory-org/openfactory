// Port of internal/metrics/{metrics,compute,facts}_test.go (the store-backed
// parts are covered by collect.test.ts).
import { describe, expect, it } from "vitest";
import type { PullRequestFacts, PullRequestRef } from "@openfactory/core";
import {
  build,
  isAutonomous,
  prFactsKey,
  runSource,
  type MEvent,
  type MInbox,
  type MetricsData,
  type MRun,
  type MWorkItem,
} from "./build";
import { FactsBook, type FactEvent } from "./facts";
import { mean, median, percentile } from "./stats";

const HOUR = 3_600_000;
const DAY = 24 * HOUR;
const day0 = Date.UTC(2026, 4, 1);
const since = day0;
const until = day0 + 7 * DAY;
const at = (d: number, h: number): string => new Date(day0 + d * DAY + h * HOUR).toISOString();
const approx = (a: number, b: number): boolean => Math.abs(a - b) < 1e-9;

const ev = (kind: string, t: string, data?: unknown): MEvent => ({ kind, created_at: t, ...(data === undefined ? {} : { data }) });
const pr = (n: number, state: string): PullRequestRef => ({ forge: "GITHUB", repo: "acme/app", number: n, url: `https://x/${n}`, branch: "", state });

function fixture(): MetricsData {
  const pr1 = pr(1, "merged");
  const pr2 = pr(2, "merged");
  const pr3 = pr(3, "open");
  const pr4 = pr(4, "closed");
  const wi = (x: MWorkItem): MWorkItem => x;
  return {
    workItems: [
      wi({ id: "w1", stage: "complete", created_at: at(0, 0), completed_at: at(0, 10), pull_requests: [pr1] }),
      wi({ id: "w2", stage: "complete", created_at: at(1, 0), completed_at: at(1, 20), pull_requests: [pr2, pr3] }),
      wi({ id: "w3", stage: "complete", created_at: at(2, 0), completed_at: at(3, 6) }),
      wi({ id: "w4", stage: "complete", created_at: new Date(since - 48 * HOUR).toISOString(), completed_at: at(0, 2) }),
      wi({ id: "w5", stage: "cancelled", created_at: at(3, 0), completed_at: at(3, 1) }),
      wi({ id: "w6", stage: "building", created_at: at(4, 0), pull_requests: [pr4] }),
      wi({ id: "w7", stage: "complete", created_at: at(5, 0), completed_at: at(5, 1), local_owner: "dev@laptop" }),
      wi({ id: "w8", stage: "complete", created_at: at(6, 0), completed_at: new Date(until + HOUR).toISOString() }),
    ],
    events: {
      w1: [
        ev("message_in", at(0, 0)),
        ev("run_started", at(0, 0)),
        ev("stage_changed", at(0, 1), { from: "intake", to: "building" }),
        ev("pr_linked", at(0, 5), pr1),
        ev("stage_changed", at(0, 9), { to: "reviewing" }),
        ev("stage_changed", at(0, 10), { to: "complete" }),
      ],
      w2: [
        ev("message_in", at(1, 0)),
        ev("run_started", at(1, 0)),
        ev("question", at(1, 2)),
        ev("pr_linked", at(1, 8), { pull_request: pr2 }),
        ev("pr_linked", at(1, 9), pr3),
      ],
      w3: [
        ev("run_started", at(2, 0)),
        ev("approval_requested", at(2, 1)),
        ev("approval_decided", at(2, 5)),
        ev("stage_changed", at(2, 3), { stage: "awaiting_approval" }),
      ],
      w6: [ev("pr_linked", at(4, 3), { note: "unparsable" })],
    },
    runs: [
      { id: "r1", work_item_id: "w1", agent_name: "foreman", state: "succeeded", triggered_by: "automation:a1", created_at: at(0, 0), config: { harness: { model: "big" } }, usage: { cost_usd: 1, input_tokens: 100, output_tokens: 10, duration_ms: 0 } },
      { id: "r1c", work_item_id: "w1", agent_name: "implement", state: "succeeded", triggered_by: "run:r1", created_at: at(0, 1), config: { harness: { model: "small" } }, usage: { cost_usd: 2, input_tokens: 200, output_tokens: 20, duration_ms: 0 } },
      { id: "r2", work_item_id: "w2", agent_name: "foreman", state: "failed", triggered_by: "api", created_at: at(1, 0), config: { harness: { model: "big" } }, usage: { cost_usd: 4, input_tokens: 400, output_tokens: 40, duration_ms: 0 } },
      { id: "r6", work_item_id: "w6", agent_name: "implement", state: "running", triggered_by: "benchmark:b1", created_at: at(4, 0), usage: { cost_usd: 0.5, input_tokens: 0, output_tokens: 0, duration_ms: 0 } },
      { id: "r4", work_item_id: "w4", agent_name: "foreman", state: "succeeded", created_at: new Date(since - 47 * HOUR).toISOString(), usage: { cost_usd: 100, input_tokens: 0, output_tokens: 0, duration_ms: 0 } },
    ] as MRun[],
    inbox: [
      { id: "i1", work_item_id: "w2", kind: "question", created_at: at(1, 2), resolved: true, resolved_at: at(1, 3) },
      { id: "i2", work_item_id: "w3", kind: "approval", created_at: at(2, 1), resolved: true, resolved_at: at(2, 5) },
      { id: "i3", work_item_id: "w6", kind: "approval", created_at: new Date(until - 2 * HOUR).toISOString() },
      { id: "i4", kind: "review", created_at: at(1, 0) },
    ] as MInbox[],
    scorers: [{ id: "s1", slug: "tests-run", name: "Tests run" }, { id: "s2", slug: "idle" }],
    scores: [
      { scorer_id: "s1", label: "tests_run", passed: true, created_at: at(0, 3) },
      { scorer_id: "s1", label: "tests_skipped", passed: false, created_at: at(0, 4) },
      { scorer_id: "s1", label: "tests_run", passed: true, created_at: at(2, 4) },
      { scorer_id: "s1", label: "tests_run", passed: true, created_at: new Date(until).toISOString() },
    ],
    facts: {},
  };
}

describe("build", () => {
  const d = build("fac", fixture(), since, until);

  it("counts work items", () => {
    expect(d.work_items).toMatchObject({ created: 7, completed: 5, cancelled: 1 });
    expect(d.work_items.by_stage).toMatchObject({ complete: 5, building: 1, cancelled: 1 });
  });

  it("computes autonomy", () => {
    const a = d.autonomy;
    expect(a.completed_work_items).toBe(5);
    expect(a.autonomous_work_items).toBe(3);
    expect(approx(a.rate, 0.6)).toBe(true);
    expect(a.merged_prs).toBe(0);
    expect(a.merged_prs_without_facts).toBe(2);
    expect(a.pr_rate).toBe(0);
    expect(a.facts_since).toBeUndefined();
  });

  it("computes cycle time and stage medians", () => {
    const c = d.cycle_time;
    expect(c.count).toBe(5);
    expect(c.median_ms).toBe(20 * HOUR);
    expect(c.p90_ms).toBe(50 * HOUR);
    expect(c.mean_ms).toBe(Math.trunc((111 * HOUR) / 5));
    expect(c.stage_median_ms["building"]).toBe(8 * HOUR);
    expect(c.stage_median_ms["reviewing"]).toBe(HOUR);
    expect(c.stage_median_ms["intake"]).toBe(2 * HOUR);
    expect(c.stage_median_ms["awaiting_approval"]).toBe(27 * HOUR);
  });

  it("computes pull request stats", () => {
    const p = d.pull_requests;
    expect(p).toMatchObject({ opened: 4, merged: 2, open: 1, closed: 1, merge_rate: 0.5 });
    expect(approx(p.median_cost_usd, 2)).toBe(true);
    expect(approx(p.total_cost_usd, 7.5)).toBe(true);
    expect(p.most_expensive.map((x) => x.number)).toEqual([1, 2, 3, 4]);
    expect(p.most_expensive[3]!.opened_at).toBe(at(4, 3));
    expect(p.median_time_to_pr_ms).toBe(6.5 * HOUR);
  });

  it("computes human wait", () => {
    const h = d.human_wait;
    expect(h).toMatchObject({ items: 3, pending: 1, median_ms: (HOUR + 4 * HOUR) / 2, p90_ms: 4 * HOUR, total_ms: 7 * HOUR });
    expect(h.by_kind["approval"]).toBe(2);
    expect(h.by_kind["review"]).toBeUndefined();
  });

  it("computes runs and cost", () => {
    const r = d.runs;
    expect(r.total).toBe(4);
    expect(r.by_agent).toMatchObject({ foreman: 2, implement: 2 });
    expect(r.by_state["succeeded"]).toBe(2);
    expect(r.by_agent_state["foreman"]!["failed"]).toBe(1);
    expect(r.by_source).toEqual({ AUTOMATION: 1, BENCHMARK: 1, ORCHESTRATION: 1, API: 1 });
    expect(r.by_model["(default)"]).toBe(1);
    const c = d.cost;
    expect(approx(c.total.cost_usd, 7.5)).toBe(true);
    expect(c.total.input_tokens).toBe(700);
    expect(c.total.runs).toBe(4);
    expect(approx(c.by_agent["foreman"]!.cost_usd, 5)).toBe(true);
    expect(approx(c.by_model["big"]!.cost_usd, 5)).toBe(true);
    expect(c.by_model["small"]!.output_tokens).toBe(20);
  });

  it("computes scorer pass rates", () => {
    expect(d.scorers).toHaveLength(2);
    const s1 = d.scorers.find((s) => s.scorer_id === "s1")!;
    expect(s1).toMatchObject({ total: 3, passed: 2, name: "Tests run" });
    expect(approx(s1.pass_rate, 2 / 3)).toBe(true);
    expect(s1.labels["tests_run"]).toBe(2);
  });

  it("builds the daily series", () => {
    expect(d.daily).toHaveLength(7);
    expect(d.daily[0]!.date).toBe("2026-05-01");
    expect(d.daily[6]!.date).toBe("2026-05-07");
    const p0 = d.daily[0]!;
    expect(p0).toMatchObject({ runs: 2, work_items_created: 1, work_items_completed: 2, prs_opened: 1, prs_merged: 1, scores: 2, scores_passed: 1 });
    expect(approx(p0.cost_usd, 3)).toBe(true);
    expect(d.daily[1]).toMatchObject({ prs_opened: 2, prs_merged: 1 });
    expect(d.daily.reduce((n, p) => n + p.runs, 0)).toBe(d.runs.total);
  });
});

describe("autonomy rules", () => {
  const w: MWorkItem = { id: "w", stage: "complete", created_at: at(0, 0) };
  const e = (...kinds: string[]): MEvent[] => kinds.map((k) => ({ kind: k, created_at: at(0, 0) }));
  const i = (...kinds: string[]): MInbox[] => kinds.map((k) => ({ kind: k as MInbox["kind"], created_at: at(0, 0) }));
  const cases: [string, MWorkItem, MEvent[], MInbox[], boolean][] = [
    ["no events", w, [], [], true],
    ["initial message only", w, e("message_in", "run_started", "message_out"), [], true],
    ["follow-up message after start", w, e("run_started", "message_in"), [], false],
    ["two messages before start", w, e("message_in", "message_in"), [], false],
    ["approvals allowed", w, e("approval_requested", "approval_decided"), i("approval", "review"), true],
    ["answer", w, e("answer"), [], false],
    ["handoff inbox", w, [], i("handoff"), false],
    ["failure inbox", w, [], i("failure"), false],
    ["local takeover", { ...w, local_owner: "x" }, [], [], false],
  ];
  it.each(cases)("%s", (_n, item, events, inbox, want) => {
    expect(isAutonomous(item, events, inbox)).toBe(want);
  });
});

describe("percentiles", () => {
  const cases: [number[], number, number, number][] = [
    [[], 0, 0, 0],
    [[5], 5, 5, 5],
    [[4, 1, 3, 2], 2, 4, 2],
    [[1, 2, 3, 4, 5, 6, 7, 8, 9, 10], 5, 9, 5],
  ];
  it.each(cases)("%j", (xs, med, p90, m) => {
    expect(median(xs)).toBe(med);
    expect(percentile(xs, 90)).toBe(p90);
    expect(mean(xs)).toBe(m);
  });
});

it("deduplicates a pull request across work items", () => {
  const open = pr(9, "open");
  const merged = { ...open, repo: "a/b", state: "merged" };
  const o = { ...open, repo: "a/b" };
  const d: MetricsData = {
    ...fixture(),
    workItems: [
      { id: "x", stage: "building", created_at: at(2, 0), pull_requests: [o] },
      { id: "y", stage: "building", created_at: at(1, 0), pull_requests: [merged] },
    ],
    events: {},
    runs: [
      { id: "a", work_item_id: "x", state: "succeeded", created_at: at(2, 0), usage: { cost_usd: 1, input_tokens: 0, output_tokens: 0, duration_ms: 0 } },
      { id: "b", work_item_id: "y", state: "succeeded", created_at: at(1, 0), usage: { cost_usd: 2, input_tokens: 0, output_tokens: 0, duration_ms: 0 } },
    ] as MRun[],
    inbox: [],
    scorers: [],
    scores: [],
  };
  const db = build("f", d, since, until);
  expect(db.pull_requests.opened).toBe(1);
  expect(db.pull_requests.merged).toBe(1);
  expect(approx(db.pull_requests.total_cost_usd, 3)).toBe(true);
  expect(db.pull_requests.most_expensive[0]).toMatchObject({ work_item_id: "y", opened_at: at(1, 0) });
});

it("builds an empty dashboard that serializes", () => {
  const db = build("f", null, since, until);
  expect(db.runs.total).toBe(0);
  expect(db.daily).toHaveLength(7);
  expect(db.pull_requests.most_expensive).toEqual([]);
  expect(db.scorers).toEqual([]);
  expect(() => JSON.stringify(db)).not.toThrow();
});

describe("run sources", () => {
  const cases: Record<string, string> = {
    "": "API",
    api: "API",
    mcp: "MCP",
    mention: "MENTION",
    follow_up: "MENTION",
    "automation:a1": "AUTOMATION",
    "schedule:s1": "SCHEDULE",
    "run:r1": "ORCHESTRATION",
    "scorer:x": "SCORER",
    self_improvement: "SELF_IMPROVEMENT",
    "something_else:42": "API",
  };
  it.each(Object.entries(cases))("%j is %s", (triggeredBy, want) => {
    expect(runSource({ id: "r", state: "running", created_at: at(0, 0), triggered_by: triggeredBy })).toBe(want);
  });
});

describe("compute time", () => {
  it("sums session seconds by agent and shape", () => {
    const big = { os: "linux", arch: "x86_64", vcpus: 4, memory_gb: 8 };
    const d: MetricsData = {
      ...fixture(),
      workItems: [],
      events: {},
      inbox: [],
      scorers: [],
      scores: [],
      runs: [
        { id: "r1", agent_name: "implement", state: "succeeded", created_at: at(1, 0), usage: { cost_usd: 2, input_tokens: 0, output_tokens: 0, duration_ms: 0 } },
        { id: "r2", agent_name: "triage", state: "succeeded", created_at: at(1, 1), usage: { cost_usd: 1, input_tokens: 0, output_tokens: 0, duration_ms: 0 } },
        { id: "r3", agent_name: "triage", state: "running", created_at: at(2, 0) },
      ] as MRun[],
      sessions: {
        r1: [
          { instance_shape: big, compute_seconds: 600, assigned_at: at(1, 0), ended_at: at(1, 5) },
          { instance_shape: big, compute_seconds: 60 },
        ],
        r2: [{ assigned_at: at(1, 1), started_at: at(1, 1), ended_at: new Date(Date.parse(at(1, 1)) + 120_000).toISOString() }],
        r3: [{ assigned_at: new Date(until - HOUR).toISOString(), started_at: new Date(until - HOUR).toISOString() }],
      },
    };
    const c = build("f", d, since, until).cost;
    expect(approx(c.total.compute_seconds, 660 + 120 + 3600)).toBe(true);
    expect(approx(c.by_agent["implement"]!.compute_seconds, 660)).toBe(true);
    expect(approx(c.by_agent["triage"]!.compute_seconds, 3720)).toBe(true);
    expect(c.compute_by_shape["linux/x86_64 4 vCPU 8 GB"]).toEqual({ sessions: 2, seconds: 660 });
    expect(c.compute_by_shape["default"]).toEqual({ sessions: 2, seconds: 3720 });
    expect(approx(c.total.cost_usd, 3)).toBe(true);
  });
});

describe("facts", () => {
  const t0 = Date.parse(at(0, 1));
  const iso = (n: number): string => new Date(t0 + n * HOUR).toISOString();
  const agent = { handle: "of[bot]", is_bot: true, is_self: true };
  const person = { handle: "dana", is_bot: false };
  const ci = { handle: "ci[bot]", is_bot: true };
  const src = { provider: "github", kind: "pull_request", repo: "acme/app", number: 1, url: "https://x/1" };
  const prEvent = (type: string, actor: FactEvent["actor"], h: number, attrs: Record<string, unknown> = {}): FactEvent => ({
    team_id: "team1", provider: "github", type, actor, source: src, attributes: attrs, received_at: iso(h),
  });
  const push = (actor: FactEvent["actor"], h: number, sha: string): FactEvent => ({
    team_id: "team1", provider: "github", type: "push", actor, source: { provider: "github", kind: "push", repo: "acme/app" },
    attributes: { branch: "fix", head_sha: sha }, received_at: iso(h),
  });

  it("records an autonomous pull request", () => {
    const book = new FactsBook();
    const steps: FactEvent[] = [
      prEvent("pull_request_opened", agent, 0, { head_branch: "fix", additions: 30, deletions: 5 }),
      push(ci, 1, "s1"),
      prEvent("pull_request_synchronized", ci, 1, { head_sha: "s1" }),
      prEvent("pull_request_review_submitted", agent, 2),
      prEvent("pull_request_review_submitted", person, 3),
      prEvent("pull_request_review_submitted", person, 4),
      prEvent("pull_request_merged", person, 5, { merged_at: iso(5) }),
      push(person, 6, "s2"),
      { team_id: "team1", provider: "slack", type: "message", actor: person, source: { provider: "slack", kind: "pull_request", repo: "x", number: 9 }, attributes: {}, received_at: iso(0) },
    ];
    for (const s of steps) book.record(s, Date.parse(s.received_at));
    const f = book.get("team1", "github:acme/app#1")!;
    expect(f.opener_kind).toBe("agent");
    expect(f.opened_at).toBe(iso(0));
    expect(f.head_branch).toBe("fix");
    expect(f.pushes).toHaveLength(1);
    expect(f.pushes![0]).toMatchObject({ kind: "bot", sha: "s1" });
    expect(f.first_review_at).toBe(iso(3));
    expect(f.merged_at).toBe(iso(5));
    expect(f.additions! + f.deletions!).toBe(35);
    expect(book.list()).toHaveLength(1);
  });

  it("feeds the autonomy and cycle figures of build", () => {
    const adds = 5;
    const dels = 2;
    const facts: Record<string, PullRequestFacts> = {
      [prFactsKey({ forge: "GITHUB", repo: "acme/app", number: 1 })]: {
        id: "t|1", team_id: "t", key: "github:acme/app#1", provider: "github", repo: "acme/app", number: 1, updated_at: at(0, 0),
        opener_kind: "agent", opened_at: at(0, 4), first_review_at: at(0, 6), merged_at: at(0, 9),
        pushes: [{ at: at(0, 5), kind: "bot" }], additions: adds, deletions: dels,
      },
      [prFactsKey({ forge: "GITHUB", repo: "Acme/App", number: 2 })]: {
        id: "t|2", team_id: "t", key: "github:acme/app#2", provider: "github", repo: "acme/app", number: 2, updated_at: at(0, 0),
        opener_kind: "agent", opened_at: at(1, 8), merged_at: at(1, 18),
        pushes: [{ at: at(1, 10), kind: "person" }], additions: 400,
      },
    };
    const d = build("fac", { ...fixture(), facts }, since, until);
    const a = d.autonomy;
    expect(a).toMatchObject({ merged_prs: 2, autonomous_merged_prs: 1, pr_rate: 0.5, merged_prs_without_facts: 0, facts_since: at(0, 4) });
    expect(a.completed_work_items).toBe(5);
    const pc = d.cycle_time.pull_requests;
    expect(pc).toMatchObject({
      count: 2, reviewed: 1, start_to_pr_ms: 6 * HOUR, pr_to_first_review_ms: 2 * HOUR,
      first_review_to_merge_ms: 3 * HOUR, start_to_merge_ms: (9 * HOUR + 18 * HOUR) / 2,
    });
    const sizes = Object.fromEntries(d.pull_requests.by_size.map((b) => [b.size, b]));
    expect(sizes["xs"]!.count).toBe(1);
    expect(sizes["l"]!.count).toBe(1);
    expect(sizes["unknown"]!.count).toBe(2);
    expect(sizes["xl"]!.lines).toBe("1000+");
    expect(sizes["s"]!.lines).toBe("10-49");
    expect(approx(sizes["xs"]!.total_cost_usd, 3)).toBe(true);
  });
});
