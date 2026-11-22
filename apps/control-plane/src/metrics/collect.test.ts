import { describe, expect, it } from "vitest";
import type { PullRequestFacts } from "@openfactory/core";
import { DEFAULT_LOOKBACK_MS, prFactsKey, type Dashboard } from "./build";
import { collect, compute, loadFacts, partitionSessionLoader, MAX_SESSION_BATCH, type IndexList } from "./collect";
import { GOLDEN_BUILD } from "./testdata/golden-build";
import { newRoutedId, newToken } from "@openfactory/core";
import { factsKeyOf, pushFactsKey, type FactEvent } from "./facts";
import { GOLDEN_SINCE, GOLDEN_UNTIL, fakeIndex, goldenSet, type ListStats } from "./testdata/golden";

const DAY = 86_400_000;
const day0 = Date.UTC(2026, 4, 1);
const iso = (ms: number) => new Date(ms).toISOString();

/** A fake IndexDO.list: newest first, 3 items per page. */
function fake(items: Record<string, Record<string, unknown>[]>, calls: string[]): IndexList {
  return async (op, p) => {
    calls.push(op);
    let all = [...(items[op] ?? [])];
    if (p["sort_order"] !== "asc") all = all.reverse();
    const start = p["cursor"] ? Number(p["cursor"]) : 0;
    const page = all.slice(start, start + 3);
    const next = start + 3 < all.length ? String(start + 3) : "";
    return { items: page, page_info: next ? { has_next_page: true, next_cursor: next } : { has_next_page: false } };
  };
}

describe("collect", () => {
  it("pages, stops at the lookback and builds the dashboard", async () => {
    const calls: string[] = [];
    const runs = Array.from({ length: 10 }, (_, i) => ({
      id: `r${i}`,
      state: "succeeded",
      agent_name: "a",
      created_at: iso(day0 + i * 1000),
      usage: { cost_usd: 1, input_tokens: 1, output_tokens: 1, duration_ms: 0 },
    }));
    runs.unshift({
      id: "ancient",
      state: "succeeded",
      agent_name: "a",
      created_at: iso(day0 - 400 * DAY),
      usage: { cost_usd: 100, input_tokens: 0, output_tokens: 0, duration_ms: 0 },
    });
    const list = fake(
      {
        runs,
        workItems: [
          {
            id: "w1",
            stage: "complete",
            created_at: iso(day0),
            completed_at: iso(day0 + 3600_000),
            updated_at: iso(day0),
            pull_requests: [{ forge: "GITHUB", repo: "o/r", number: 1, url: "", branch: "", state: "merged" }],
          },
        ],
        metricEvents: [
          {
            id: "e1",
            work_item_id: "w1",
            kind: "pr_linked",
            at_ms: day0 + 1000,
            data: { repo: "o/r", number: 1, forge: "GITHUB" },
          },
        ],
        prFacts: [
          {
            team_id: "t",
            provider: "github",
            type: "pull_request_opened",
            actor: { handle: "bot", is_bot: true, is_self: true },
            source: { provider: "github", kind: "pull_request", repo: "o/r", number: 1 },
            attributes: {},
            received_at: iso(day0 + 2000),
          },
          {
            team_id: "t",
            provider: "github",
            type: "pull_request_merged",
            actor: { handle: "p", is_bot: false },
            source: { provider: "github", kind: "pull_request", repo: "o/r", number: 1 },
            attributes: {},
            received_at: iso(day0 + 3000),
          },
        ],
        scores: [{ scorer_id: "s", label: "x", passed: true, created_at: iso(day0 + 5) }],
        inbox: [],
      },
      calls,
    );
    const d = await compute(list, "fac", [{ id: "s", slug: "s" } as never], day0, day0 + 7 * DAY);
    expect(d.runs.total).toBe(10);
    expect(d.cost.total.cost_usd).toBe(10);
    expect(d.work_items).toMatchObject({ created: 1, completed: 1 });
    expect(d.pull_requests).toMatchObject({ opened: 1, merged: 1 });
    expect(d.autonomy).toMatchObject({ merged_prs: 1, autonomous_merged_prs: 1 });
    expect(d.scorers[0]).toMatchObject({ total: 1, passed: 1 });
    expect(calls.filter((c) => c === "runs").length).toBeGreaterThanOrEqual(3);
  });
  it("rejects an inverted window and a missing factory", async () => {
    await expect(collect(fake({}, []), "f", [], 2, 1)).rejects.toThrow(/until/);
    await expect(collect(fake({}, []), "", [], 1, 2)).rejects.toThrow(/factory/);
  });

  it("loads the inbox of the lookback and the events of the linked pull requests only", async () => {
    const g = goldenSet();
    const from = GOLDEN_SINCE - DEFAULT_LOOKBACK_MS;
    const stats: ListStats = {};
    const d = await collect(fakeIndex(g, stats), "fac_golden", g.scorers, GOLDEN_SINCE, GOLDEN_UNTIL);
    const wanted = new Set<string>();
    for (const w of d.workItems)
      for (const pr of w.pull_requests ?? []) if (pr.repo && pr.number) wanted.add(prFactsKey(pr));

    // Before: every inbox item of the factory and every forge event of the team.
    const inboxInLookback = g.data.inbox.filter((it) => Date.parse(it.created_at) >= from).length;
    expect(inboxInLookback).toBeLessThan(g.data.inbox.length);
    expect(stats["inbox"]!.rows).toBe(inboxInLookback);
    expect(d.inbox).toHaveLength(inboxInLookback);

    const linkedEvents = g.factEvents.filter((e) => wanted.has(e.prKey)).length;
    expect(linkedEvents).toBeLessThan(g.factEvents.length);
    expect(stats["prFacts"]!.rows).toBe(linkedEvents);
    // One call per pull request and one per distinct head branch (for its pushes).
    const branches = new Set<string>();
    for (const e of g.factEvents) {
      const b = e.event.attributes?.["head_branch"];
      const repo = (e.event.source as { repo: string }).repo;
      if (wanted.has(e.prKey) && typeof b === "string" && b) branches.add(pushFactsKey(e.event.provider, repo, b));
    }
    expect(branches.size).toBeGreaterThan(0);
    expect(stats["prFacts"]!.calls).toBe(wanted.size + branches.size);
  });

  it("gives the same facts per pull request as the full replay", async () => {
    const g = goldenSet();
    const wanted = new Set(Object.keys(g.data.facts));
    wanted.add("github:nobody/none#1");
    const strip = (fs: Record<string, PullRequestFacts>) =>
      Object.fromEntries(Object.entries(fs).map(([k, f]) => [k, { ...f, updated_at: "" }]));
    const fullStats: ListStats = {};
    const full = await loadFacts(fakeIndex(g, fullStats), wanted, 0);
    const perKey = await loadFacts(fakeIndex(g), wanted);
    expect(fullStats["prFacts"]!.rows).toBe(g.factEvents.length);
    expect(Object.keys(perKey).length).toBeGreaterThan(10);
    expect(strip(perKey)).toEqual(strip(full));
    expect(strip(perKey)).toEqual(strip(g.data.facts));
  });
});

describe("push facts", () => {
  const t0 = Date.UTC(2026, 4, 1);
  let seq = 0;
  const ev = (
    type: string,
    atMin: number,
    src: Record<string, unknown>,
    attrs: Record<string, unknown> = {},
  ): FactEvent => ({
    id: `ev${String(seq++).padStart(3, "0")}`,
    team_id: "t",
    provider: "github",
    type,
    actor: { handle: "dev", is_bot: false },
    source: { provider: "github", ...src },
    attributes: attrs,
    received_at: iso(t0 + atMin * 60_000),
  });
  const pr = (n: number) => ({ kind: "pull_request", repo: "acme/app", number: n });
  const push = (repo: string, branch: string, sha: string) => ({ kind: "push", repo, branch, sha });
  const pushEv = (atMin: number, p: ReturnType<typeof push>) =>
    ev("push", atMin, { kind: "push", repo: p.repo }, { branch: p.branch, head_sha: p.sha });

  const events: FactEvent[] = [
    pushEv(0, push("acme/app", "feat/a", "s0")), // before the pull request: skipped
    ev("pull_request_opened", 1, pr(1), { head_branch: "feat/a" }),
    pushEv(2, push("acme/app", "feat/a", "s2")), // counted for #1
    pushEv(3, push("other/app", "feat/a", "s3")), // other repository
    pushEv(3, push("acme/app", "feat/b", "s3b")), // other branch
    ev("pull_request_merged", 4, pr(1), { head_branch: "feat/a" }),
    pushEv(5, push("acme/app", "feat/a", "s5")), // #1 is merged, #2 not open yet
    ev("pull_request_opened", 6, pr(2), { head_branch: "feat/a" }),
    pushEv(7, push("acme/app", "feat/a", "s7")), // counted for #2
  ];

  /** A fake IndexDO prFacts list: rows under factsKeyOf, ordered by time then ID. */
  const index: IndexList = async (_op, p) => {
    let rows = events.filter((e) => factsKeyOf(e) !== undefined);
    if (p["pr_key"]) rows = rows.filter((e) => factsKeyOf(e) === p["pr_key"]);
    return { items: rows, page_info: { has_next_page: false } };
  };

  it("stores a push under its branch and a pull request event under its pull request", () => {
    expect(factsKeyOf(events[0]!)).toBe("push:github:acme/app@feat/a");
    expect(factsKeyOf(events[1]!)).toBe(prFactsKey({ forge: "GITHUB", repo: "acme/app", number: 1 }));
    expect(factsKeyOf({ ...events[0]!, provider: "Azure-DevOps" })).toBe("push:azure_devops:acme/app@feat/a");
    expect(factsKeyOf({ ...events[0]!, attributes: {} })).toBeUndefined();
    expect(factsKeyOf({ ...events[0]!, provider: "slack" })).toBeUndefined();
    expect(factsKeyOf({ ...events[1]!, source: { repo: "acme/app" } })).toBeUndefined();
  });

  it("replays the pushes of the head branch with the events of each pull request", async () => {
    const wanted = new Set([1, 2].map((n) => prFactsKey({ forge: "GITHUB", repo: "acme/app", number: n })));
    const strip = (fs: Record<string, PullRequestFacts>) =>
      Object.fromEntries(Object.entries(fs).map(([k, f]) => [k, { ...f, updated_at: "" }]));
    const perKey = await loadFacts(index, wanted);
    const full = await loadFacts(index, wanted, 0);
    const shas = (n: number) =>
      (perKey[prFactsKey({ forge: "GITHUB", repo: "acme/app", number: n })]?.pushes ?? []).map((x) => x.sha);
    expect(shas(1)).toEqual(["s2"]);
    expect(shas(2)).toEqual(["s7"]);
    expect(strip(perKey)).toEqual(strip(full));
  });
});

describe("sessions", () => {
  it("adds the compute time of the run sessions, as Go's collectSessions did", async () => {
    const g = goldenSet();
    const asked: string[] = [];
    const sessions = async (runs: readonly { id: string }[]) => {
      asked.push(...runs.map((r) => r.id));
      return Object.fromEntries(
        runs.filter((r) => g.data.sessions?.[r.id]).map((r) => [r.id, g.data.sessions![r.id]!]),
      );
    };
    const without = await compute(fakeIndex(g), "fac_golden", g.scorers, GOLDEN_SINCE, GOLDEN_UNTIL);
    expect(without.cost.compute_by_shape).toEqual({});
    const got = await compute(fakeIndex(g), "fac_golden", g.scorers, GOLDEN_SINCE, GOLDEN_UNTIL, sessions);
    // Only the runs created in the window.
    for (const id of asked) {
      const r = g.data.runs.find((x) => x.id === id)!;
      expect(Date.parse(r.created_at)).toBeGreaterThanOrEqual(GOLDEN_SINCE);
    }
    expect(Object.keys(got.cost.compute_by_shape).length).toBeGreaterThan(1);
    // The same costs as build() over all sessions; only the order of the float sums differs.
    const round = (v: unknown): unknown =>
      JSON.parse(JSON.stringify(v, (_k, x: unknown) => (typeof x === "number" ? Math.round(x * 1e4) / 1e4 : x)));
    expect(round(got.cost)).toEqual(round((GOLDEN_BUILD as Dashboard).cost));
  });

  it("groups the runs by work partition and batches the calls", async () => {
    const t1 = newToken("w");
    const t2 = newToken("w");
    const runs = [
      ...Array.from({ length: MAX_SESSION_BATCH + 5 }, () => ({
        id: newRoutedId("run", t1),
        state: "succeeded",
        created_at: "",
      })),
      { id: newRoutedId("run", t2), state: "succeeded", created_at: "" },
      { id: "run_without_partition", state: "succeeded", created_at: "" },
    ];
    const calls: { partition: string; n: number }[] = [];
    const load = partitionSessionLoader(async (partition, ids) => {
      calls.push({ partition, n: ids.length });
      return Object.fromEntries(ids.map((id, i) => [id, i === 0 ? [{ compute_seconds: 1 }] : []]));
    });
    const out = await load(runs as never);
    expect(calls.sort((a, b) => b.n - a.n)).toEqual([
      { partition: t1, n: MAX_SESSION_BATCH },
      { partition: t1, n: 5 },
      { partition: t2, n: 1 },
    ]);
    // Runs without sessions are left out.
    expect(Object.keys(out)).toHaveLength(3);
  });
});
