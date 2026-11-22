import { describe, expect, it } from "vitest";
import type { PullRequestFacts } from "@openfactory/core";
import { DEFAULT_LOOKBACK_MS, prFactsKey } from "./build";
import { collect, compute, loadFacts, type IndexList } from "./collect";
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
    expect(stats["prFacts"]!.calls).toBe(wanted.size);
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
