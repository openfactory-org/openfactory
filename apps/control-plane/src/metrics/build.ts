// Factory dashboard metrics: port of Build in internal/metrics/metrics.go.
// Build is pure and deterministic. See the definitions in ./README.md (the
// package documentation of internal/metrics/doc.go).
//
// All ranges are half-open [since, until) in UTC. Timestamps are RFC 3339
// strings in the inputs and in the dashboard, like the Go JSON.
import {
  isTerminalStage,
  personPushedBefore,
  pullRequestChangedLines,
  pullRequestSource,
  runSourceAndRef,
  sourceKey,
  type InboxItem,
  type InboxKind,
  type PullRequestFacts,
  type PullRequestRef,
  type Run,
  type RunState,
  type Scorer,
  type Score,
  type Stage,
  type WorkItem,
  type WorkItemEvent,
} from "@openfactory/core";
import { mean, median, medianFloat, percentile, ratio } from "./stats";

/** How far before `since` the collector loads work items and runs (90 days). */
export const DEFAULT_LOOKBACK_MS = 90 * 24 * 3600 * 1000;
export const MOST_EXPENSIVE_LIMIT = 5;
const MAX_DAILY_POINTS = 3660;
const DAY_MS = 86_400_000;

// ---------- Input ----------

export type MWorkItem = Pick<WorkItem, "id" | "stage" | "created_at"> &
  Partial<Pick<WorkItem, "completed_at" | "updated_at" | "pull_requests" | "local_owner">>;
export type MEvent = Pick<WorkItemEvent, "kind" | "created_at"> & Partial<Pick<WorkItemEvent, "data" | "id" | "work_item_id">>;
export type MRun = Pick<Run, "id" | "state" | "created_at"> &
  Partial<Pick<Run, "work_item_id" | "agent_name" | "usage" | "triggered_by" | "source" | "source_ref">> & {
    config?: { name?: string; harness?: { model?: string } };
  };
export type MInbox = Pick<InboxItem, "kind" | "created_at"> & Partial<Pick<InboxItem, "id" | "work_item_id" | "resolved" | "resolved_at">>;
export type MScorer = Pick<Scorer, "id"> & Partial<Pick<Scorer, "slug" | "name">>;
export type MScore = Pick<Score, "scorer_id" | "created_at"> & Partial<Pick<Score, "label" | "passed">>;

/** The sandbox session of a run, for the compute time. */
export interface MSession {
  instance_shape?: { os?: string; arch?: string; vcpus?: number; memory_gb?: number } | null;
  compute_seconds?: number;
  assigned_at?: string;
  started_at?: string;
  ended_at?: string;
}

export interface MetricsData {
  workItems: MWorkItem[];
  /** Work-item timelines by work item ID. */
  events: Record<string, MEvent[]>;
  runs: MRun[];
  inbox: MInbox[];
  scorers: MScorer[];
  scores: MScore[];
  /** Pull request facts by PRFactsKey (lower-case source key). */
  facts: Record<string, PullRequestFacts>;
  /** Sessions by run ID. */
  sessions?: Record<string, MSession[]>;
}

export function emptyData(): MetricsData {
  return { workItems: [], events: {}, runs: [], inbox: [], scorers: [], scores: [], facts: {} };
}

// ---------- Output ----------

export interface PRCost {
  forge?: string;
  repo: string;
  number: number;
  url?: string;
  state: string;
  work_item_id: string;
  opened_at: string;
  cost_usd: number;
}

export interface PRSizeCost {
  size: string;
  lines: string;
  count: number;
  total_cost_usd: number;
  median_cost_usd: number;
}

export interface UsageTotals {
  runs: number;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  duration_ms: number;
  compute_seconds: number;
}

export interface ComputeStats {
  sessions: number;
  seconds: number;
}

export interface ScorerStats {
  scorer_id: string;
  slug?: string;
  name?: string;
  total: number;
  passed: number;
  pass_rate: number;
  labels: Record<string, number>;
}

export interface DailyPoint {
  date: string;
  runs: number;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  work_items_created: number;
  work_items_completed: number;
  prs_opened: number;
  prs_merged: number;
  scores: number;
  scores_passed: number;
}

export interface Dashboard {
  factory_id: string;
  since: string;
  until: string;
  work_items: { created: number; completed: number; cancelled: number; by_stage: Record<string, number> };
  pull_requests: {
    opened: number;
    merged: number;
    closed: number;
    open: number;
    merge_rate: number;
    median_cost_usd: number;
    total_cost_usd: number;
    most_expensive: PRCost[];
    median_time_to_pr_ms: number;
    by_size: PRSizeCost[];
  };
  autonomy: {
    completed_work_items: number;
    autonomous_work_items: number;
    rate: number;
    merged_prs: number;
    autonomous_merged_prs: number;
    pr_rate: number;
    merged_prs_without_facts: number;
    facts_since?: string;
  };
  cycle_time: {
    count: number;
    median_ms: number;
    p90_ms: number;
    mean_ms: number;
    stage_median_ms: Record<string, number>;
    pull_requests: {
      count: number;
      start_to_merge_ms: number;
      start_to_pr_ms: number;
      pr_to_first_review_ms: number;
      first_review_to_merge_ms: number;
      reviewed: number;
    };
  };
  human_wait: { items: number; pending: number; total_ms: number; median_ms: number; p90_ms: number; by_kind: Record<string, number> };
  runs: {
    total: number;
    by_agent: Record<string, number>;
    by_state: Record<string, number>;
    by_agent_state: Record<string, Record<string, number>>;
    by_model: Record<string, number>;
    by_source: Record<string, number>;
  };
  cost: {
    total: UsageTotals;
    by_agent: Record<string, UsageTotals>;
    by_model: Record<string, UsageTotals>;
    compute_by_shape: Record<string, ComputeStats>;
  };
  scorers: ScorerStats[];
  daily: DailyPoint[];
}

// ---------- Helpers ----------

const ms = (t: string | undefined): number => (t ? Date.parse(t) : NaN);
const iso = (t: number): string => new Date(t).toISOString();
const inWindow = (t: number, since: number, until: number): boolean => t >= since && t < until;

function bump(m: Record<string, number>, k: string, by = 1): void {
  m[k] = (m[k] ?? 0) + by;
}

function newUsage(): UsageTotals {
  return { runs: 0, cost_usd: 0, input_tokens: 0, output_tokens: 0, duration_ms: 0, compute_seconds: 0 };
}

function addUsage(u: UsageTotals, x: MRun["usage"]): void {
  u.runs++;
  u.cost_usd += x?.cost_usd ?? 0;
  u.input_tokens += x?.input_tokens ?? 0;
  u.output_tokens += x?.output_tokens ?? 0;
  u.duration_ms += x?.duration_ms ?? 0;
}

/** InstanceShape.Label of the Go code. */
export function shapeLabel(s: MSession["instance_shape"]): string {
  if (!s || (!s.os && !s.arch && !s.vcpus && !s.memory_gb)) return "default";
  const parts: string[] = [];
  if (s.os && s.arch) parts.push(`${s.os}/${s.arch}`);
  else if (s.os) parts.push(s.os);
  else if (s.arch) parts.push(s.arch);
  if (s.vcpus && s.vcpus > 0) parts.push(`${s.vcpus} vCPU`);
  if (s.memory_gb && s.memory_gb > 0) parts.push(`${s.memory_gb} GB`);
  return parts.join(" ");
}

/** The sandbox time of a session. A session that did not end counts until the end of the window. */
export function sessionSeconds(s: MSession, untilMs: number): number {
  if ((s.compute_seconds ?? 0) > 0) return s.compute_seconds!;
  let from = ms(s.assigned_at);
  if (s.started_at) from = ms(s.started_at);
  const end = s.ended_at ? ms(s.ended_at) : untilMs;
  if (Number.isNaN(from) || from <= 0 || !(end > from)) return 0;
  return (end - from) / 1000;
}

// ---------- Autonomy ----------

/** The autonomy definition of the package documentation. */
export function isAutonomous(w: Pick<MWorkItem, "local_owner">, events: readonly MEvent[], inbox: readonly Pick<MInbox, "kind">[]): boolean {
  if (w.local_owner) return false;
  for (const it of inbox) {
    if (it.kind === "question" || it.kind === "handoff" || it.kind === "failure") return false;
  }
  let started = false;
  let firstMessage = false;
  for (const e of events) {
    switch (e.kind) {
      case "question":
      case "answer":
        return false;
      case "run_started":
        started = true;
        break;
      case "message_in":
        if (started || firstMessage) return false;
        firstMessage = true;
        break;
    }
  }
  return true;
}

// ---------- Stages ----------

function stageFromEvent(e: MEvent): Stage | "" {
  const d = e.data;
  if (d === undefined || d === null || d === "") return "";
  if (typeof d === "string") return d as Stage;
  if (typeof d !== "object" || Array.isArray(d)) return "";
  const m = d as Record<string, unknown>;
  for (const k of ["to", "stage", "new_stage"]) {
    const v = m[k];
    if (typeof v === "string" && v !== "") return v as Stage;
  }
  return "";
}

/** Time spent per stage, from stage_changed events. The item starts in intake at its creation. */
export function stageDurations(w: MWorkItem, events: readonly MEvent[]): Record<string, number> | null {
  const out: Record<string, number> = {};
  let cur: Stage = "intake";
  let at = ms(w.created_at);
  let seen = false;
  for (const e of events) {
    if (e.kind !== "stage_changed") continue;
    const to = stageFromEvent(e);
    if (to === "") continue;
    seen = true;
    const d = ms(e.created_at) - at;
    if (d > 0) bump(out, cur, d);
    cur = to;
    at = ms(e.created_at);
  }
  if (!seen) return null;
  if (w.completed_at && !isTerminalStage(cur)) {
    const d = ms(w.completed_at) - at;
    if (d > 0) bump(out, cur, d);
  }
  return out;
}

// ---------- Pull requests ----------

type PRRef = Pick<PullRequestRef, "repo" | "number"> & Partial<Pick<PullRequestRef, "forge" | "url" | "state" | "branch">>;

export function prKey(pr: PRRef): string {
  if (pr.repo && pr.number) return `${pr.forge ?? ""}|${pr.repo}#${pr.number}`.toLowerCase();
  return (pr.url ?? "").toLowerCase();
}

/** The key of Data.facts for a pull request. */
export function prFactsKey(pr: PRRef): string {
  return sourceKey(pullRequestSource({ forge: "", url: "", branch: "", state: "", ...pr } as PullRequestRef)).toLowerCase();
}

interface LinkTimes {
  byKey: Map<string, number>;
  anon: number;
  hasAnon: boolean;
}

function findLink(l: LinkTimes, pr: PRRef): number | undefined {
  const a = l.byKey.get(prKey(pr));
  if (a !== undefined) return a;
  if (pr.url) {
    const b = l.byKey.get(pr.url.toLowerCase());
    if (b !== undefined) return b;
  }
  return l.hasAnon ? l.anon : undefined;
}

function validPR(v: unknown): PRRef | undefined {
  if (typeof v !== "object" || v === null || Array.isArray(v)) return undefined;
  const o = v as Record<string, unknown>;
  const pr: PRRef = {
    forge: typeof o["forge"] === "string" ? o["forge"] : "",
    repo: typeof o["repo"] === "string" ? o["repo"] : "",
    number: typeof o["number"] === "number" ? o["number"] : 0,
    url: typeof o["url"] === "string" ? o["url"] : "",
    state: typeof o["state"] === "string" ? o["state"] : "",
    branch: typeof o["branch"] === "string" ? o["branch"] : "",
  };
  return pr.url || (pr.repo && pr.number) ? pr : undefined;
}

/** Reads a pull request ref from event data, at the top level or under "pull_request" / "pr". */
export function parsePR(data: unknown): PRRef | undefined {
  const top = validPR(data);
  if (top) return top;
  if (typeof data !== "object" || data === null || Array.isArray(data)) return undefined;
  const wrap = data as Record<string, unknown>;
  for (const k of ["pull_request", "pr"]) {
    const inner = validPR(wrap[k]);
    if (inner) return inner;
  }
  return undefined;
}

function prLinkTimes(events: readonly MEvent[]): LinkTimes {
  const lt: LinkTimes = { byKey: new Map(), anon: 0, hasAnon: false };
  for (const e of events) {
    if (e.kind !== "pr_linked") continue;
    const at = ms(e.created_at);
    const keys: string[] = [];
    const pr = parsePR(e.data);
    if (pr) {
      const k = prKey(pr);
      if (k !== "") keys.push(k);
      if (pr.url) keys.push(pr.url.toLowerCase());
    }
    if (keys.length === 0) {
      if (!lt.hasAnon || at < lt.anon) {
        lt.anon = at;
        lt.hasAnon = true;
      }
      continue;
    }
    for (const k of keys) {
      const t = lt.byKey.get(k);
      if (t === undefined || at < t) lt.byKey.set(k, at);
    }
  }
  return lt;
}

function stateRank(s: string): number {
  switch (s.toLowerCase()) {
    case "merged":
      return 3;
    case "closed":
      return 2;
    case "open":
      return 1;
  }
  return 0;
}

interface PREntry {
  forge: string;
  repo: string;
  number: number;
  url: string;
  state: string;
  workItemId: string;
  openedMs: number;
  costUsd: number;
  wiCreated: number;
  facts?: PullRequestFacts;
}

function collectPRs(
  items: readonly MWorkItem[],
  events: Record<string, MEvent[]>,
  wiCost: Map<string, number>,
  facts: Record<string, PullRequestFacts>,
): PREntry[] {
  const byKey = new Map<string, PREntry>();
  const order: string[] = [];
  for (const w of items) {
    const prs = w.pull_requests ?? [];
    if (prs.length === 0) continue;
    const linked = prLinkTimes(events[w.id] ?? []);
    const share = (wiCost.get(w.id) ?? 0) / prs.length;
    const created = ms(w.created_at);
    for (const pr of prs) {
      const key = prKey(pr);
      if (key === "") continue;
      const opened = findLink(linked, pr) ?? created;
      const e = byKey.get(key);
      if (!e) {
        byKey.set(key, {
          forge: pr.forge ?? "",
          repo: pr.repo,
          number: pr.number,
          url: pr.url ?? "",
          state: pr.state ?? "",
          workItemId: w.id,
          openedMs: opened,
          costUsd: share,
          wiCreated: created,
        });
        order.push(key);
        continue;
      }
      // Seen on another work item: keep the earliest observation, sum cost.
      e.costUsd += share;
      if (opened < e.openedMs) {
        e.openedMs = opened;
        e.workItemId = w.id;
        e.wiCreated = created;
      }
      if (stateRank(pr.state ?? "") > stateRank(e.state)) e.state = pr.state ?? "";
    }
  }
  const out: PREntry[] = [];
  for (const k of order) {
    const e = byKey.get(k)!;
    const f = facts[prFactsKey({ forge: e.forge, repo: e.repo, number: e.number })];
    if (f) {
      e.facts = f;
      // The forge knows when the PR opened.
      if (f.opened_at) e.openedMs = ms(f.opened_at);
      if (f.merged_at) e.state = "merged";
    }
    out.push(e);
  }
  return out;
}

const PR_SIZES: readonly { size: string; max: number }[] = [
  { size: "xs", max: 10 },
  { size: "s", max: 50 },
  { size: "m", max: 250 },
  { size: "l", max: 1000 },
  { size: "xl", max: 0 },
];

function prSize(f: PullRequestFacts | undefined): string {
  if (!f) return "unknown";
  const n = pullRequestChangedLines(f);
  if (n === null) return "unknown";
  for (const s of PR_SIZES) if (s.max === 0 || n < s.max) return s.size;
  return "xl";
}

function sizeCost(size: string, lines: string, xs: readonly number[] | undefined): PRSizeCost {
  const list = xs ?? [];
  let total = 0;
  for (const x of list) total += x;
  return { size, lines, count: list.length, total_cost_usd: total, median_cost_usd: medianFloat(list) };
}

function sizeBuckets(costs: Map<string, number[]>): PRSizeCost[] {
  const out: PRSizeCost[] = [];
  let lo = 0;
  for (const s of PR_SIZES) {
    const lines = s.max === 0 ? `${lo}+` : `${lo}-${s.max - 1}`;
    out.push(sizeCost(s.size, lines, costs.get(s.size)));
    lo = s.max;
  }
  out.push(sizeCost("unknown", "", costs.get("unknown")));
  return out;
}

function buildPRFacts(db: Dashboard, prs: readonly PREntry[], since: number, until: number): void {
  const startToMerge: number[] = [];
  const startToPR: number[] = [];
  const prToReview: number[] = [];
  const reviewToMerge: number[] = [];
  let factsSince: number | undefined;
  const diff = (a: number, b: number): number | undefined => (b < a ? undefined : b - a);
  for (const p of prs) {
    const f = p.facts;
    if (f?.opened_at) {
      const t = ms(f.opened_at);
      if (factsSince === undefined || t < factsSince) factsSince = t;
    }
    if (!f || !f.merged_at) {
      if (!f && p.state.toLowerCase() === "merged" && inWindow(p.openedMs, since, until)) db.autonomy.merged_prs_without_facts++;
      continue;
    }
    const merged = ms(f.merged_at);
    if (!inWindow(merged, since, until)) continue;
    db.autonomy.merged_prs++;
    if (!personPushedBefore(f, new Date(merged))) db.autonomy.autonomous_merged_prs++;
    db.cycle_time.pull_requests.count++;
    const v = diff(p.wiCreated, merged);
    if (v !== undefined) startToMerge.push(v);
    const toPR = diff(p.wiCreated, p.openedMs);
    if (toPR !== undefined) startToPR.push(toPR);
    if (f.first_review_at) {
      db.cycle_time.pull_requests.reviewed++;
      const rev = ms(f.first_review_at);
      const a = diff(p.openedMs, rev);
      if (a !== undefined) prToReview.push(a);
      const b = diff(rev, merged);
      if (b !== undefined) reviewToMerge.push(b);
    }
  }
  if (factsSince !== undefined) db.autonomy.facts_since = iso(factsSince);
  const c = db.cycle_time.pull_requests;
  c.start_to_merge_ms = median(startToMerge);
  c.start_to_pr_ms = median(startToPR);
  c.pr_to_first_review_ms = median(prToReview);
  c.first_review_to_merge_ms = median(reviewToMerge);
}

// ---------- Runs ----------

const runAgent = (r: MRun): string => r.agent_name || r.config?.name || "(unknown)";
const runModel = (r: MRun): string => r.config?.harness?.model || "(default)";
const runSource = (r: MRun): string => runSourceAndRef({ source: r.source, source_ref: r.source_ref, triggered_by: r.triggered_by }).source;

/** The source of a run for the metrics ("API" for an empty or unknown triggered_by). */
export { runSource };

// ---------- Build ----------

function sortedEvents(ev: readonly MEvent[] | undefined): MEvent[] {
  return [...(ev ?? [])].sort((a, b) => ms(a.created_at) - ms(b.created_at));
}

/** Derives the dashboard from raw data for [since, until). */
export function build(factoryId: string, input: MetricsData | null | undefined, sinceMs: number, untilMs: number): Dashboard {
  const d = input ?? emptyData();
  const since = sinceMs;
  const until = untilMs;
  const db: Dashboard = {
    factory_id: factoryId,
    since: iso(since),
    until: iso(until),
    work_items: { created: 0, completed: 0, cancelled: 0, by_stage: {} },
    pull_requests: {
      opened: 0,
      merged: 0,
      closed: 0,
      open: 0,
      merge_rate: 0,
      median_cost_usd: 0,
      total_cost_usd: 0,
      most_expensive: [],
      median_time_to_pr_ms: 0,
      by_size: [],
    },
    autonomy: {
      completed_work_items: 0,
      autonomous_work_items: 0,
      rate: 0,
      merged_prs: 0,
      autonomous_merged_prs: 0,
      pr_rate: 0,
      merged_prs_without_facts: 0,
    },
    cycle_time: {
      count: 0,
      median_ms: 0,
      p90_ms: 0,
      mean_ms: 0,
      stage_median_ms: {},
      pull_requests: { count: 0, start_to_merge_ms: 0, start_to_pr_ms: 0, pr_to_first_review_ms: 0, first_review_to_merge_ms: 0, reviewed: 0 },
    },
    human_wait: { items: 0, pending: 0, total_ms: 0, median_ms: 0, p90_ms: 0, by_kind: {} },
    runs: { total: 0, by_agent: {}, by_state: {}, by_agent_state: {}, by_model: {}, by_source: {} },
    cost: { total: newUsage(), by_agent: {}, by_model: {}, compute_by_shape: {} },
    scorers: [],
    daily: [],
  };

  // Daily series.
  const startDay = Math.floor(since / DAY_MS) * DAY_MS;
  const daily: DailyPoint[] = [];
  for (let t = startDay; t < until && daily.length < MAX_DAILY_POINTS; t += DAY_MS) {
    daily.push({
      date: iso(t).slice(0, 10),
      runs: 0,
      cost_usd: 0,
      input_tokens: 0,
      output_tokens: 0,
      work_items_created: 0,
      work_items_completed: 0,
      prs_opened: 0,
      prs_merged: 0,
      scores: 0,
      scores_passed: 0,
    });
  }
  const at = (t: number, fn: (p: DailyPoint) => void): void => {
    const day = Math.floor(t / DAY_MS) * DAY_MS;
    if (day < startDay) return;
    const p = daily[(day - startDay) / DAY_MS];
    if (p) fn(p);
  };

  const inboxByWI = new Map<string, MInbox[]>();
  for (const it of d.inbox) {
    if (it.work_item_id) {
      const l = inboxByWI.get(it.work_item_id) ?? [];
      l.push(it);
      inboxByWI.set(it.work_item_id, l);
    }
  }

  // ----- Work items, autonomy, cycle time -----
  const cycle: number[] = [];
  const stageDur = new Map<string, number[]>();
  for (const w of d.workItems) {
    const ev = sortedEvents(d.events[w.id]);
    const autonomous = isAutonomous(w, ev, inboxByWI.get(w.id) ?? []);
    const created = ms(w.created_at);
    if (inWindow(created, since, until)) {
      db.work_items.created++;
      bump(db.work_items.by_stage, w.stage);
      at(created, (p) => p.work_items_created++);
    }
    if (!w.completed_at) continue;
    const completed = ms(w.completed_at);
    if (!inWindow(completed, since, until)) continue;
    if (w.stage === "cancelled") {
      db.work_items.cancelled++;
    } else if (w.stage === "complete") {
      db.work_items.completed++;
      at(completed, (p) => p.work_items_completed++);
      db.autonomy.completed_work_items++;
      if (autonomous) db.autonomy.autonomous_work_items++;
      const dur = completed - created;
      if (dur >= 0) cycle.push(dur);
      const sd = stageDurations(w, ev);
      if (sd) {
        for (const [st, v] of Object.entries(sd)) {
          const l = stageDur.get(st) ?? [];
          l.push(v);
          stageDur.set(st, l);
        }
      }
    }
  }
  db.autonomy.rate = ratio(db.autonomy.autonomous_work_items, db.autonomy.completed_work_items);
  db.cycle_time.count = cycle.length;
  db.cycle_time.median_ms = median(cycle);
  db.cycle_time.p90_ms = percentile(cycle, 90);
  db.cycle_time.mean_ms = mean(cycle);
  for (const [st, xs] of stageDur) db.cycle_time.stage_median_ms[st] = median(xs);

  // ----- Run cost per work item -----
  const wiCost = new Map<string, number>();
  for (const r of d.runs) {
    if (r.work_item_id) wiCost.set(r.work_item_id, (wiCost.get(r.work_item_id) ?? 0) + (r.usage?.cost_usd ?? 0));
  }

  // ----- Pull requests -----
  const prs = collectPRs(d.workItems, d.events, wiCost, d.facts);
  const prCosts: number[] = [];
  const timeToPR: number[] = [];
  const sizeCosts = new Map<string, number[]>();
  const expensive: PRCost[] = [];
  for (const p of prs) {
    if (!inWindow(p.openedMs, since, until)) continue;
    const pr = db.pull_requests;
    pr.opened++;
    prCosts.push(p.costUsd);
    pr.total_cost_usd += p.costUsd;
    const cost: PRCost = {
      repo: p.repo,
      number: p.number,
      state: p.state,
      work_item_id: p.workItemId,
      opened_at: iso(p.openedMs),
      cost_usd: p.costUsd,
    };
    if (p.forge) cost.forge = p.forge;
    if (p.url) cost.url = p.url;
    expensive.push(cost);
    if (p.wiCreated <= p.openedMs) timeToPR.push(p.openedMs - p.wiCreated);
    const size = prSize(p.facts);
    sizeCosts.set(size, [...(sizeCosts.get(size) ?? []), p.costUsd]);
    const merged = p.state.toLowerCase() === "merged";
    if (merged) pr.merged++;
    else if (p.state.toLowerCase() === "closed") pr.closed++;
    else pr.open++;
    at(p.openedMs, (dp) => {
      dp.prs_opened++;
      if (merged) dp.prs_merged++;
    });
  }
  db.pull_requests.merge_rate = ratio(db.pull_requests.merged, db.pull_requests.opened);
  db.pull_requests.median_cost_usd = medianFloat(prCosts);
  db.pull_requests.median_time_to_pr_ms = median(timeToPR);
  // Stable sort: cost descending, then opened earlier first.
  expensive.sort((a, b) => (a.cost_usd !== b.cost_usd ? b.cost_usd - a.cost_usd : Date.parse(a.opened_at) - Date.parse(b.opened_at)));
  db.pull_requests.most_expensive = expensive.slice(0, MOST_EXPENSIVE_LIMIT);
  db.pull_requests.by_size = sizeBuckets(sizeCosts);
  buildPRFacts(db, prs, since, until);
  db.autonomy.pr_rate = ratio(db.autonomy.autonomous_merged_prs, db.autonomy.merged_prs);

  // ----- Human wait -----
  const waits: number[] = [];
  for (const it of d.inbox) {
    const created = ms(it.created_at);
    if (it.kind === "review" || !inWindow(created, since, until)) continue;
    db.human_wait.items++;
    bump(db.human_wait.by_kind, it.kind as InboxKind);
    if (it.resolved && it.resolved_at) {
      const w = Math.max(0, ms(it.resolved_at) - created);
      waits.push(w);
      db.human_wait.total_ms += w;
    } else {
      db.human_wait.pending++;
      const w = until - created;
      if (w > 0) db.human_wait.total_ms += w;
    }
  }
  db.human_wait.median_ms = median(waits);
  db.human_wait.p90_ms = percentile(waits, 90);

  // ----- Runs and cost -----
  for (const r of d.runs) {
    const created = ms(r.created_at);
    if (!inWindow(created, since, until)) continue;
    const agent = runAgent(r);
    const model = runModel(r);
    db.runs.total++;
    bump(db.runs.by_agent, agent);
    bump(db.runs.by_state, r.state as RunState);
    const byState = (db.runs.by_agent_state[agent] ??= {});
    bump(byState, r.state);
    bump(db.runs.by_model, model);
    bump(db.runs.by_source, runSource(r));

    let compute = 0;
    for (const ss of d.sessions?.[r.id] ?? []) {
      const sec = sessionSeconds(ss, until);
      compute += sec;
      const label = shapeLabel(ss.instance_shape);
      const c = (db.cost.compute_by_shape[label] ??= { sessions: 0, seconds: 0 });
      c.sessions++;
      c.seconds += sec;
    }
    addUsage(db.cost.total, r.usage);
    db.cost.total.compute_seconds += compute;
    const ua = (db.cost.by_agent[agent] ??= newUsage());
    addUsage(ua, r.usage);
    ua.compute_seconds += compute;
    const um = (db.cost.by_model[model] ??= newUsage());
    addUsage(um, r.usage);
    um.compute_seconds += compute;

    at(created, (p) => {
      p.runs++;
      p.cost_usd += r.usage?.cost_usd ?? 0;
      p.input_tokens += r.usage?.input_tokens ?? 0;
      p.output_tokens += r.usage?.output_tokens ?? 0;
    });
  }

  // ----- Scorers -----
  const byScorer = new Map<string, ScorerStats>();
  for (const sc of d.scorers) {
    const st: ScorerStats = { scorer_id: sc.id, total: 0, passed: 0, pass_rate: 0, labels: {} };
    if (sc.slug) st.slug = sc.slug;
    if (sc.name) st.name = sc.name;
    byScorer.set(sc.id, st);
  }
  for (const s of d.scores) {
    const created = ms(s.created_at);
    if (!inWindow(created, since, until)) continue;
    let st = byScorer.get(s.scorer_id);
    if (!st) {
      st = { scorer_id: s.scorer_id, total: 0, passed: 0, pass_rate: 0, labels: {} };
      byScorer.set(s.scorer_id, st);
    }
    st.total++;
    if (s.passed) st.passed++;
    bump(st.labels, s.label ?? "");
    at(created, (p) => {
      p.scores++;
      if (s.passed) p.scores_passed++;
    });
  }
  db.scorers = [...byScorer.values()];
  for (const st of db.scorers) st.pass_rate = ratio(st.passed, st.total);
  db.scorers.sort((a, b) => {
    const sa = a.slug ?? "";
    const sb = b.slug ?? "";
    if (sa !== sb) return sa < sb ? -1 : 1;
    return a.scorer_id < b.scorer_id ? -1 : a.scorer_id > b.scorer_id ? 1 : 0;
  });

  db.daily = daily;
  return db;
}
