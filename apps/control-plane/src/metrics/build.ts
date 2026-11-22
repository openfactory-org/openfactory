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
  type PullRequestState,
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
export type MEvent = Pick<WorkItemEvent, "kind" | "created_at"> &
  Partial<Pick<WorkItemEvent, "data" | "id" | "work_item_id">>;
export type MRun = Pick<Run, "id" | "state" | "created_at"> &
  Partial<Pick<Run, "work_item_id" | "agent_name" | "usage" | "triggered_by" | "source" | "source_ref">> & {
    config?: { name?: string; harness?: { model?: string } };
  };
export type MInbox = Pick<InboxItem, "kind" | "created_at"> &
  Partial<Pick<InboxItem, "id" | "work_item_id" | "resolved" | "resolved_at">>;
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
  human_wait: {
    items: number;
    pending: number;
    total_ms: number;
    median_ms: number;
    p90_ms: number;
    by_kind: Record<string, number>;
  };
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
export function isAutonomous(
  w: Pick<MWorkItem, "local_owner">,
  events: readonly MEvent[],
  inbox: readonly Pick<MInbox, "kind">[],
): boolean {
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

type PRRef = Pick<PullRequestRef, "repo" | "number"> &
  Partial<Pick<PullRequestRef, "forge" | "url" | "state" | "branch">>;

export function prKey(pr: PRRef): string {
  if (pr.repo && pr.number) return `${pr.forge ?? ""}|${pr.repo}#${pr.number}`.toLowerCase();
  return (pr.url ?? "").toLowerCase();
}

/** The key of Data.facts for a pull request. */
export function prFactsKey(pr: PRRef): string {
  return sourceKey(
    pullRequestSource({ forge: "", url: "", branch: "", state: "", ...pr } as PullRequestRef),
  ).toLowerCase();
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
    // Event data is not checked: an unknown state ranks lowest (stateRank).
    state: typeof o["state"] === "string" ? (o["state"] as PullRequestState) : "",
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

// ---------- Runs ----------

const runAgent = (r: MRun): string => r.agent_name || r.config?.name || "(unknown)";
const runModel = (r: MRun): string => r.config?.harness?.model || "(default)";
const runSource = (r: MRun): string =>
  runSourceAndRef({ source: r.source, source_ref: r.source_ref, triggered_by: r.triggered_by }).source;

/** The source of a run for the metrics ("API" for an empty or unknown triggered_by). */
export { runSource };

// ---------- Daily series ----------

/** One point per UTC day of [since, until), at most MAX_DAILY_POINTS. */
class DailySeries {
  readonly points: DailyPoint[] = [];
  private readonly startDay: number;

  constructor(since: number, until: number) {
    this.startDay = Math.floor(since / DAY_MS) * DAY_MS;
    for (let t = this.startDay; t < until && this.points.length < MAX_DAILY_POINTS; t += DAY_MS) {
      this.points.push({
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
  }

  /** Applies fn to the point of the day of t. A time outside the series changes nothing. */
  at(t: number, fn: (p: DailyPoint) => void): void {
    const day = Math.floor(t / DAY_MS) * DAY_MS;
    if (day < this.startDay) return;
    const p = this.points[(day - this.startDay) / DAY_MS];
    if (p) fn(p);
  }
}

// ---------- Sections ----------
//
// Each section reads the input and returns its part of the dashboard. A
// section that has a daily column also adds to the daily series. build()
// calls the sections in a fixed order, so floating-point sums keep their
// order and the output is stable.

/** The window of a build. */
interface BuildWindow {
  since: number;
  until: number;
  daily: DailySeries;
}

interface WorkItemSection {
  work_items: Dashboard["work_items"];
  autonomy: Pick<Dashboard["autonomy"], "completed_work_items" | "autonomous_work_items" | "rate">;
  cycle_time: Omit<Dashboard["cycle_time"], "pull_requests">;
}

function sortedEvents(ev: readonly MEvent[] | undefined): MEvent[] {
  return [...(ev ?? [])].sort((a, b) => ms(a.created_at) - ms(b.created_at));
}

function inboxByWorkItem(inbox: readonly MInbox[]): Map<string, MInbox[]> {
  const out = new Map<string, MInbox[]>();
  for (const it of inbox) {
    if (!it.work_item_id) continue;
    const l = out.get(it.work_item_id);
    if (l) l.push(it);
    else out.set(it.work_item_id, [it]);
  }
  return out;
}

/** Work items created and completed in the window, their autonomy and their cycle time. */
function workItemSection(d: MetricsData, w: BuildWindow): WorkItemSection {
  const { since, until, daily } = w;
  const work_items: Dashboard["work_items"] = { created: 0, completed: 0, cancelled: 0, by_stage: {} };
  let completedItems = 0;
  let autonomousItems = 0;
  const cycle: number[] = [];
  const stageDur = new Map<string, number[]>();
  const inbox = inboxByWorkItem(d.inbox);
  for (const wi of d.workItems) {
    const ev = sortedEvents(d.events[wi.id]);
    const created = ms(wi.created_at);
    if (inWindow(created, since, until)) {
      work_items.created++;
      bump(work_items.by_stage, wi.stage);
      daily.at(created, (p) => p.work_items_created++);
    }
    if (!wi.completed_at) continue;
    const completed = ms(wi.completed_at);
    if (!inWindow(completed, since, until)) continue;
    if (wi.stage === "cancelled") {
      work_items.cancelled++;
      continue;
    }
    if (wi.stage !== "complete") continue;
    work_items.completed++;
    daily.at(completed, (p) => p.work_items_completed++);
    completedItems++;
    if (isAutonomous(wi, ev, inbox.get(wi.id) ?? [])) autonomousItems++;
    const dur = completed - created;
    if (dur >= 0) cycle.push(dur);
    const sd = stageDurations(wi, ev);
    if (!sd) continue;
    for (const [st, v] of Object.entries(sd)) {
      const l = stageDur.get(st);
      if (l) l.push(v);
      else stageDur.set(st, [v]);
    }
  }
  const stage_median_ms: Record<string, number> = {};
  for (const [st, xs] of stageDur) stage_median_ms[st] = median(xs);
  return {
    work_items,
    autonomy: {
      completed_work_items: completedItems,
      autonomous_work_items: autonomousItems,
      rate: ratio(autonomousItems, completedItems),
    },
    cycle_time: {
      count: cycle.length,
      median_ms: median(cycle),
      p90_ms: percentile(cycle, 90),
      mean_ms: mean(cycle),
      stage_median_ms,
    },
  };
}

/** Run cost per work item, over all runs of the input. */
function costPerWorkItem(runs: readonly MRun[]): Map<string, number> {
  const out = new Map<string, number>();
  for (const r of runs) {
    if (r.work_item_id) out.set(r.work_item_id, (out.get(r.work_item_id) ?? 0) + (r.usage?.cost_usd ?? 0));
  }
  return out;
}

/** Pull requests opened in the window: counts, cost and size. */
function pullRequestSection(prs: readonly PREntry[], w: BuildWindow): Dashboard["pull_requests"] {
  const { since, until, daily } = w;
  let opened = 0;
  let merged = 0;
  let closed = 0;
  let open = 0;
  let total = 0;
  const prCosts: number[] = [];
  const timeToPR: number[] = [];
  const sizeCosts = new Map<string, number[]>();
  const expensive: PRCost[] = [];
  for (const p of prs) {
    if (!inWindow(p.openedMs, since, until)) continue;
    opened++;
    prCosts.push(p.costUsd);
    total += p.costUsd;
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
    const bucket = sizeCosts.get(size);
    if (bucket) bucket.push(p.costUsd);
    else sizeCosts.set(size, [p.costUsd]);
    const isMerged = p.state.toLowerCase() === "merged";
    if (isMerged) merged++;
    else if (p.state.toLowerCase() === "closed") closed++;
    else open++;
    daily.at(p.openedMs, (dp) => {
      dp.prs_opened++;
      if (isMerged) dp.prs_merged++;
    });
  }
  // Stable sort: cost descending, then opened earlier first.
  expensive.sort((a, b) =>
    a.cost_usd !== b.cost_usd ? b.cost_usd - a.cost_usd : Date.parse(a.opened_at) - Date.parse(b.opened_at),
  );
  return {
    opened,
    merged,
    closed,
    open,
    merge_rate: ratio(merged, opened),
    median_cost_usd: medianFloat(prCosts),
    total_cost_usd: total,
    most_expensive: expensive.slice(0, MOST_EXPENSIVE_LIMIT),
    median_time_to_pr_ms: median(timeToPR),
    by_size: sizeBuckets(sizeCosts),
  };
}

interface PRFactsSection {
  autonomy: Pick<
    Dashboard["autonomy"],
    "merged_prs" | "autonomous_merged_prs" | "pr_rate" | "merged_prs_without_facts" | "facts_since"
  >;
  cycle: Dashboard["cycle_time"]["pull_requests"];
}

/** Pull requests merged in the window, from the forge facts: autonomy and cycle times. */
function prFactsSection(prs: readonly PREntry[], w: BuildWindow): PRFactsSection {
  const { since, until } = w;
  let mergedPRs = 0;
  let autonomousPRs = 0;
  let withoutFacts = 0;
  let count = 0;
  let reviewed = 0;
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
      if (!f && p.state.toLowerCase() === "merged" && inWindow(p.openedMs, since, until)) withoutFacts++;
      continue;
    }
    const merged = ms(f.merged_at);
    if (!inWindow(merged, since, until)) continue;
    mergedPRs++;
    if (!personPushedBefore(f, new Date(merged))) autonomousPRs++;
    count++;
    const v = diff(p.wiCreated, merged);
    if (v !== undefined) startToMerge.push(v);
    const toPR = diff(p.wiCreated, p.openedMs);
    if (toPR !== undefined) startToPR.push(toPR);
    if (f.first_review_at) {
      reviewed++;
      const rev = ms(f.first_review_at);
      const a = diff(p.openedMs, rev);
      if (a !== undefined) prToReview.push(a);
      const b = diff(rev, merged);
      if (b !== undefined) reviewToMerge.push(b);
    }
  }
  const autonomy: PRFactsSection["autonomy"] = {
    merged_prs: mergedPRs,
    autonomous_merged_prs: autonomousPRs,
    pr_rate: ratio(autonomousPRs, mergedPRs),
    merged_prs_without_facts: withoutFacts,
  };
  if (factsSince !== undefined) autonomy.facts_since = iso(factsSince);
  return {
    autonomy,
    cycle: {
      count,
      start_to_merge_ms: median(startToMerge),
      start_to_pr_ms: median(startToPR),
      pr_to_first_review_ms: median(prToReview),
      first_review_to_merge_ms: median(reviewToMerge),
      reviewed,
    },
  };
}

/** Inbox items (not reviews) created in the window and how long they waited. */
function humanWaitSection(inbox: readonly MInbox[], w: BuildWindow): Dashboard["human_wait"] {
  const { since, until } = w;
  const out: Dashboard["human_wait"] = { items: 0, pending: 0, total_ms: 0, median_ms: 0, p90_ms: 0, by_kind: {} };
  const waits: number[] = [];
  for (const it of inbox) {
    const created = ms(it.created_at);
    if (it.kind === "review" || !inWindow(created, since, until)) continue;
    out.items++;
    bump(out.by_kind, it.kind as InboxKind);
    if (it.resolved && it.resolved_at) {
      const wait = Math.max(0, ms(it.resolved_at) - created);
      waits.push(wait);
      out.total_ms += wait;
    } else {
      out.pending++;
      const wait = until - created;
      if (wait > 0) out.total_ms += wait;
    }
  }
  out.median_ms = median(waits);
  out.p90_ms = percentile(waits, 90);
  return out;
}

/** Runs created in the window: counts, usage and sandbox time. */
function runSection(d: MetricsData, w: BuildWindow): Pick<Dashboard, "runs" | "cost"> {
  const { since, until, daily } = w;
  const runs: Dashboard["runs"] = {
    total: 0,
    by_agent: {},
    by_state: {},
    by_agent_state: {},
    by_model: {},
    by_source: {},
  };
  const cost: Dashboard["cost"] = { total: newUsage(), by_agent: {}, by_model: {}, compute_by_shape: {} };
  for (const r of d.runs) {
    const created = ms(r.created_at);
    if (!inWindow(created, since, until)) continue;
    const agent = runAgent(r);
    const model = runModel(r);
    runs.total++;
    bump(runs.by_agent, agent);
    bump(runs.by_state, r.state as RunState);
    bump((runs.by_agent_state[agent] ??= {}), r.state);
    bump(runs.by_model, model);
    bump(runs.by_source, runSource(r));

    let compute = 0;
    for (const ss of d.sessions?.[r.id] ?? []) {
      const sec = sessionSeconds(ss, until);
      compute += sec;
      const c = (cost.compute_by_shape[shapeLabel(ss.instance_shape)] ??= { sessions: 0, seconds: 0 });
      c.sessions++;
      c.seconds += sec;
    }
    for (const u of [cost.total, (cost.by_agent[agent] ??= newUsage()), (cost.by_model[model] ??= newUsage())]) {
      addUsage(u, r.usage);
      u.compute_seconds += compute;
    }

    daily.at(created, (p) => {
      p.runs++;
      p.cost_usd += r.usage?.cost_usd ?? 0;
      p.input_tokens += r.usage?.input_tokens ?? 0;
      p.output_tokens += r.usage?.output_tokens ?? 0;
    });
  }
  return { runs, cost };
}

/** Scores created in the window, per scorer, sorted by slug then scorer ID. */
function scorerSection(d: MetricsData, w: BuildWindow): ScorerStats[] {
  const { since, until, daily } = w;
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
    daily.at(created, (p) => {
      p.scores++;
      if (s.passed) p.scores_passed++;
    });
  }
  const out = [...byScorer.values()];
  for (const st of out) st.pass_rate = ratio(st.passed, st.total);
  out.sort((a, b) => {
    const sa = a.slug ?? "";
    const sb = b.slug ?? "";
    if (sa !== sb) return sa < sb ? -1 : 1;
    return a.scorer_id < b.scorer_id ? -1 : a.scorer_id > b.scorer_id ? 1 : 0;
  });
  return out;
}

// ---------- Build ----------

/** Derives the dashboard from raw data for [since, until). */
export function build(
  factoryId: string,
  input: MetricsData | null | undefined,
  sinceMs: number,
  untilMs: number,
): Dashboard {
  const d = input ?? emptyData();
  const w: BuildWindow = { since: sinceMs, until: untilMs, daily: new DailySeries(sinceMs, untilMs) };
  const wi = workItemSection(d, w);
  const prs = collectPRs(d.workItems, d.events, costPerWorkItem(d.runs), d.facts);
  const pull_requests = pullRequestSection(prs, w);
  const facts = prFactsSection(prs, w);
  const human_wait = humanWaitSection(d.inbox, w);
  const { runs, cost } = runSection(d, w);
  const scorers = scorerSection(d, w);
  // The key order is the JSON order of the dashboard.
  return {
    factory_id: factoryId,
    since: iso(sinceMs),
    until: iso(untilMs),
    work_items: wi.work_items,
    pull_requests,
    autonomy: { ...wi.autonomy, ...facts.autonomy },
    cycle_time: { ...wi.cycle_time, pull_requests: facts.cycle },
    human_wait,
    runs,
    cost,
    scorers,
    daily: w.daily.points,
  };
}
