// A fixed, seeded dataset for the golden tests of build() and collect().
// The generator is deterministic: the same seed gives the same data, so the
// dashboards in golden-*.json stay valid. Do not change the generator without
// regenerating the golden files from a commit that you trust.
import type { PullRequestFacts, PullRequestRef, Scorer } from "@openfactory/core";
import {
  prFactsKey,
  type MEvent,
  type MetricsData,
  type MInbox,
  type MRun,
  type MSession,
  type MWorkItem,
} from "../build";
import type { IndexList } from "../collect";
import { FactsBook, type FactEvent } from "../facts";

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

export const GOLDEN_SINCE = Date.UTC(2026, 4, 1);
export const GOLDEN_UNTIL = GOLDEN_SINCE + 30 * DAY;
/** The clock of FactsBook.record for events without a valid received_at. */
const NOW = GOLDEN_UNTIL + DAY;

/** mulberry32: a small seeded PRNG. */
function rng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export interface GoldenSet {
  data: MetricsData;
  scorers: Scorer[];
  /** The raw forge events, oldest first, as IndexDO stores them. */
  factEvents: { prKey: string; receivedMs: number; id: string; event: FactEvent }[];
  /** Work-item timelines as IndexDO rows (work_item_metric_events), oldest first. */
  metricEvents: { id: string; work_item_id: string; kind: string; at_ms: number; data: unknown }[];
}

export function goldenSet(seed = 42): GoldenSet {
  const r = rng(seed);
  const int = (a: number, b: number): number => a + Math.floor(r() * (b - a + 1));
  const pick = <T>(xs: readonly T[]): T => xs[Math.floor(r() * xs.length)]!;
  const chance = (p: number): boolean => r() < p;
  const iso = (t: number): string => new Date(t).toISOString();
  const between = (a: number, b: number): number => a + Math.floor(r() * (b - a));
  const money = (max: number): number => Math.round(r() * max * 10_000) / 10_000;

  const lo = GOLDEN_SINCE - 110 * DAY;
  const hi = GOLDEN_UNTIL + 2 * DAY;
  /** Most rows fall in the window; the rest spread over the lookback and a little after it. */
  const when = (): number => (chance(0.6) ? between(GOLDEN_SINCE, GOLDEN_UNTIL) : between(lo, hi));

  // ----- Work items, their pull requests and timelines -----
  const stages = [
    "intake",
    "building",
    "reviewing",
    "complete",
    "complete",
    "complete",
    "cancelled",
    "awaiting_approval",
  ];
  const forges = ["GITHUB", "github", "", "GITLAB"];
  const repos = ["acme/app", "acme/api", "Acme/App"];
  const prStates: PullRequestRef["state"][] = ["open", "merged", "merged", "closed", ""];
  const workItems: MWorkItem[] = [];
  const events: Record<string, MEvent[]> = {};
  const metricEvents: GoldenSet["metricEvents"] = [];
  const allPRs: PullRequestRef[] = [];
  let evSeq = 0;
  for (let i = 0; i < 160; i++) {
    const id = `w${String(i).padStart(3, "0")}`;
    const created = when();
    const stage = pick(stages);
    const w: MWorkItem = { id, stage: stage as MWorkItem["stage"], created_at: iso(created) };
    const terminal = stage === "complete" || stage === "cancelled";
    if (terminal || chance(0.05)) w.completed_at = iso(created + between(0, 12 * DAY));
    w.updated_at = iso(w.completed_at ? Date.parse(w.completed_at) : created + between(0, 40 * DAY));
    if (chance(0.1)) w.local_owner = "dev@laptop";
    if (chance(0.45)) {
      const prs: PullRequestRef[] = [];
      for (let k = int(1, 3); k > 0; k--) {
        if (chance(0.06)) {
          prs.push({
            forge: "",
            repo: "",
            number: 0,
            url: `https://forge.test/x/${int(1, 9)}`,
            branch: "",
            state: pick(prStates),
          });
          continue;
        }
        const n = int(1, 70);
        const pr: PullRequestRef = {
          forge: pick(forges),
          repo: pick(repos),
          number: n,
          url: chance(0.5) ? `https://forge.test/pr/${n}` : "",
          branch: chance(0.5) ? `feature/${n}` : "",
          state: pick(prStates),
        };
        prs.push(pr);
      }
      w.pull_requests = prs;
      allPRs.push(...prs);
    }
    workItems.push(w);

    // Timeline.
    const tl: MEvent[] = [];
    let t = created;
    const add = (kind: string, data?: unknown): void => {
      t += between(0, 6 * HOUR);
      const e: MEvent = { kind, created_at: iso(t) };
      if (data !== undefined) e.data = data;
      tl.push(e);
    };
    if (chance(0.7)) add("message_in");
    if (chance(0.8)) add("run_started");
    if (chance(0.15)) add("message_in");
    if (chance(0.1)) add(pick(["question", "answer"]));
    const stageData = (to: string): unknown =>
      pick([{ from: "intake", to }, { stage: to }, to, { new_stage: to }, null, { other: to }]);
    for (let k = int(0, 4); k > 0; k--) add("stage_changed", stageData(pick(stages)));
    for (const pr of w.pull_requests ?? []) {
      if (chance(0.2)) continue;
      add("pr_linked", pick([pr, { pull_request: pr }, { pr }, { note: "unparsable" }]));
    }
    if (chance(0.2)) add("approval_requested");
    // Some timelines arrive out of order; build() sorts them by time.
    if (chance(0.2)) tl.reverse();
    if (tl.length > 0) {
      events[id] = tl;
      for (const e of tl)
        metricEvents.push({
          id: `e${String(evSeq++).padStart(5, "0")}`,
          work_item_id: id,
          kind: e.kind,
          at_ms: Date.parse(e.created_at),
          data: e.data ?? null,
        });
    }
  }
  metricEvents.sort((a, b) => a.at_ms - b.at_ms || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));

  // ----- Runs and sessions -----
  const agents = ["foreman", "implement", "review", ""];
  const runStates = ["succeeded", "succeeded", "failed", "cancelled", "running", "queued"];
  const triggers = ["api", "automation:a1", "run:r1", "schedule:s1", "", "user:u1"];
  const runs: MRun[] = [];
  const sessions: Record<string, MSession[]> = {};
  for (let i = 0; i < 420; i++) {
    const id = `r${String(i).padStart(3, "0")}`;
    const created = when();
    const run: MRun = { id, state: pick(runStates) as MRun["state"], created_at: iso(created) };
    if (chance(0.7)) run.work_item_id = pick(workItems).id;
    const agent = pick(agents);
    if (agent) run.agent_name = agent;
    if (chance(0.6))
      run.config = { name: chance(0.5) ? "cfg-agent" : undefined, harness: { model: pick(["big", "small", ""]) } };
    const trig = pick(triggers);
    if (trig) run.triggered_by = trig;
    if (chance(0.2)) run.source = pick(["api", "github", "schedule"]) as MRun["source"];
    if (chance(0.9))
      run.usage = {
        cost_usd: money(3),
        input_tokens: int(0, 50_000),
        output_tokens: int(0, 9_000),
        duration_ms: int(0, 600_000),
      };
    runs.push(run);
    if (chance(0.5)) {
      const ss: MSession[] = [];
      for (let k = int(1, 2); k > 0; k--) {
        const s: MSession = {};
        const shape = pick([
          null,
          { os: "linux", arch: "amd64", vcpus: 4, memory_gb: 8 },
          { os: "linux", vcpus: 2 },
          { arch: "arm64", memory_gb: 16 },
          {},
        ]);
        if (shape !== null) s.instance_shape = shape;
        if (chance(0.4)) s.compute_seconds = int(1, 5000);
        else {
          s.assigned_at = iso(created + between(0, HOUR));
          if (chance(0.6)) s.started_at = iso(created + HOUR + between(0, HOUR));
          if (chance(0.7)) s.ended_at = iso(created + 2 * HOUR + between(0, 5 * HOUR));
        }
        ss.push(s);
      }
      sessions[id] = ss;
    }
  }

  // ----- Inbox -----
  const kinds = ["question", "handoff", "failure", "review", "approval", "spend_limit", "blocked"];
  const inbox: MInbox[] = [];
  for (let i = 0; i < 140; i++) {
    const created = between(lo - 60 * DAY, hi);
    const it: MInbox = { id: `inb${i}`, kind: pick(kinds) as MInbox["kind"], created_at: iso(created) };
    if (chance(0.75)) {
      // Inbox items come after their work item.
      const w = pick(workItems);
      it.work_item_id = w.id;
      it.created_at = iso(Math.max(created, Date.parse(w.created_at) + between(0, 3 * DAY)));
    }
    if (chance(0.5)) {
      it.resolved = true;
      if (chance(0.9)) it.resolved_at = iso(Date.parse(it.created_at) + between(-HOUR, 4 * DAY));
    }
    inbox.push(it);
  }
  // IndexDO lists are newest first; the generator keeps the build() input in that order too.
  inbox.sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));

  // ----- Scorers and scores -----
  const scorers = [
    { id: "sc_b", slug: "beta", name: "Beta" },
    { id: "sc_a", slug: "alpha" },
    { id: "sc_c", name: "No slug" },
  ] as Scorer[];
  const scores: MetricsData["scores"] = [];
  for (let i = 0; i < 220; i++) {
    const s: MetricsData["scores"][number] = {
      scorer_id: pick(["sc_a", "sc_b", "sc_c", "sc_orphan"]),
      created_at: iso(between(GOLDEN_SINCE - 5 * DAY, hi)),
    };
    const label = pick(["good", "bad", "", undefined]);
    if (label !== undefined) s.label = label;
    if (chance(0.6)) s.passed = true;
    scores.push(s);
  }
  scores.sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));

  // ----- Forge events (pull request facts) -----
  const raw: { receivedMs: number; event: FactEvent }[] = [];
  const keyed = [...allPRs.filter((p) => p.repo && p.number)];
  // Pull requests that no work item of this factory links (other factories of the team).
  for (let n = 100; n < 130; n++)
    keyed.push({ forge: "GITHUB", repo: "other/repo", number: n, url: "", branch: "", state: "open" });
  const providerOf = (forge: string): string => forge.toLowerCase() || "github";
  const seenPR = new Set<string>();
  for (const pr of keyed) {
    const k = `${pr.forge}|${pr.repo}|${pr.number}`;
    if (seenPR.has(k) || chance(0.15)) continue;
    seenPR.add(k);
    const provider = providerOf(pr.forge);
    const source = { provider, kind: "pull_request", repo: pr.repo, number: pr.number, url: pr.url };
    let t = between(lo, hi);
    const bot = { handle: "factory-bot", is_bot: true, is_self: true };
    const person = { handle: pick(["ana", "ben"]), is_bot: false };
    const reviewer = { handle: pick(["cy", "dee", "ana"]), is_bot: false };
    const push = (type: string, actor: FactEvent["actor"], attributes: Record<string, unknown> = {}): void => {
      t += between(0, 30 * HOUR);
      raw.push({
        receivedMs: t,
        event: { team_id: "t1", provider, type, actor, source, attributes, received_at: iso(t) },
      });
    };
    const opener = chance(0.7) ? bot : person;
    push(
      "pull_request_opened",
      opener,
      chance(0.3)
        ? { pr_created_at: iso(t - between(0, 5 * HOUR)), head_branch: pr.branch }
        : { head_branch: pr.branch },
    );
    if (chance(0.5)) push("pull_request_synchronized", chance(0.5) ? person : bot, { head_sha: `sha${int(1, 4)}` });
    if (chance(0.3)) push("pull_request_synchronized", person, {});
    if (chance(0.6)) push("pull_request_review_submitted", chance(0.8) ? reviewer : bot, {});
    if (chance(0.5))
      push("pull_request_edited", opener, {
        additions: pick([int(0, 4), int(5, 30), int(30, 150), int(150, 700), int(700, 3000)]),
        deletions: int(0, 20),
      });
    const end = pick(["merged", "merged", "closed", "none"]);
    if (end === "merged")
      push("pull_request_merged", person, chance(0.5) ? { merged_at: iso(t + between(0, HOUR)) } : {});
    else if (end === "closed") push("pull_request_closed", person, {});
  }
  // Events that the facts skip: no team, and a provider that is not a forge.
  raw.push({
    receivedMs: GOLDEN_SINCE + DAY,
    event: {
      team_id: "",
      provider: "github",
      type: "pull_request_merged",
      actor: { handle: "x", is_bot: false },
      source: { provider: "github", kind: "pull_request", repo: "acme/app", number: 1 },
      attributes: {},
      received_at: iso(GOLDEN_SINCE + DAY),
    },
  });
  raw.sort((a, b) => a.receivedMs - b.receivedMs);
  const factEvents: GoldenSet["factEvents"] = raw.map((x, i) => {
    const src = x.event.source as { repo: string; number: number };
    return {
      id: `fe${String(i).padStart(5, "0")}`,
      receivedMs: x.receivedMs,
      prKey: prFactsKey({ forge: x.event.provider.toUpperCase(), repo: src.repo, number: src.number }),
      event: x.event,
    };
  });

  const book = new FactsBook();
  for (const fe of factEvents) book.record(fe.event, NOW);
  const wanted = new Set<string>();
  for (const w of workItems)
    for (const pr of w.pull_requests ?? []) if (pr.repo && pr.number) wanted.add(prFactsKey(pr));
  const facts: Record<string, PullRequestFacts> = {};
  for (const f of book.list()) if (wanted.has(f.key.toLowerCase())) facts[f.key.toLowerCase()] = f;

  return {
    data: { workItems, events, runs, inbox, scorers, scores, facts, sessions },
    scorers,
    factEvents,
    metricEvents,
  };
}

/** Counts of what a fake IndexDO returned, per list op. */
export type ListStats = Record<string, { calls: number; rows: number }>;

/**
 * A fake IndexRpc.list over a golden set. It honours the parameters that
 * IndexDO honours for these ops (factory_id is implied, since_ms, pr_key,
 * sort_order, limit, cursor), so tests can count what a collector loads.
 */
export function fakeIndex(g: GoldenSet, stats: ListStats = {}): IndexList {
  const ms = (t: string | undefined): number => (t ? Date.parse(t) : NaN);
  const tables: Record<string, { t: number; id: string; row: unknown; prKey?: string }[]> = {
    workItems: g.data.workItems.map((w) => ({ t: ms(w.created_at), id: w.id, row: w })),
    runs: g.data.runs.map((x) => ({ t: ms(x.created_at), id: x.id, row: x })),
    inbox: g.data.inbox.map((x, i) => ({ t: ms(x.created_at), id: x.id ?? `i${i}`, row: x })),
    scores: g.data.scores.map((x, i) => ({ t: ms(x.created_at), id: `s${String(i).padStart(4, "0")}`, row: x })),
    metricEvents: g.metricEvents.map((x) => ({ t: x.at_ms, id: x.id, row: x })),
    prFacts: g.factEvents.map((x) => ({ t: x.receivedMs, id: x.id, row: x.event, prKey: x.prKey })),
  };
  return async (op, p) => {
    const st = (stats[op] ??= { calls: 0, rows: 0 });
    st.calls++;
    const asc = p["sort_order"] === "asc";
    let rows = [...(tables[op] ?? [])];
    if (p["since_ms"]) rows = rows.filter((x) => x.t >= Number(p["since_ms"]));
    if (p["pr_key"]) rows = rows.filter((x) => x.prKey === p["pr_key"]);
    rows.sort((a, b) => (a.t - b.t || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)) * (asc ? 1 : -1));
    const limit = Number(p["limit"] ?? "50");
    const start = p["cursor"] ? Number(p["cursor"]) : 0;
    const page = rows.slice(start, start + limit);
    st.rows += page.length;
    const next = start + limit < rows.length ? String(start + limit) : "";
    return {
      items: page.map((x) => x.row),
      page_info: next ? { has_next_page: true, next_cursor: next } : { has_next_page: false },
    };
  };
}
