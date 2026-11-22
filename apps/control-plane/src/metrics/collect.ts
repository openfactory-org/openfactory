// Collects the inputs of build() from the read model (IndexDO lists) and the
// scorers (TeamDO). Port of Collect/Compute in internal/metrics.
import { OfError, partitionOf, type PullRequestFacts, type Scorer, type WorkItemEvent } from "@openfactory/core";
import {
  DEFAULT_LOOKBACK_MS,
  build,
  emptyData,
  prFactsKey,
  type Dashboard,
  type MetricsData,
  type MWorkItem,
  type MRun,
  type MInbox,
  type MScore,
  type MSession,
} from "./build";
import { FactsBook, pushFactsKey, type FactEvent } from "./facts";

/** IndexRpc.list(meta, op, params) with the meta bound. */
export type IndexList = (op: string, params: Record<string, string>) => Promise<unknown>;

/** Loads the sessions of runs, by run ID. Runs without sessions can be left out. */
export type SessionLoader = (runs: readonly MRun[]) => Promise<Record<string, MSession[]>>;

/** WorkDO.api metricsSessions of one work partition: at most MAX_SESSION_BATCH run IDs. */
export type PartitionSessions = (partition: string, runIds: string[]) => Promise<Record<string, MSession[]>>;

/** How many run IDs one metricsSessions call takes (MAX_METRICS_RUNS of WorkDO). */
export const MAX_SESSION_BATCH = 200;
const SESSION_CONCURRENCY = 8;

/**
 * A SessionLoader over WorkDO: the runs are grouped by the work partition of
 * their ID (the WorkDO that has their sessions), with one call per partition
 * and MAX_SESSION_BATCH runs. A run ID without a partition has no sessions.
 */
export function partitionSessionLoader(call: PartitionSessions): SessionLoader {
  return async (runs) => {
    const byPartition = new Map<string, string[]>();
    for (const r of runs) {
      let p: string;
      try {
        p = partitionOf(r.id);
      } catch {
        continue;
      }
      const ids = byPartition.get(p) ?? [];
      ids.push(r.id);
      byPartition.set(p, ids);
    }
    const batches: [string, string[]][] = [];
    for (const [p, ids] of byPartition)
      for (let i = 0; i < ids.length; i += MAX_SESSION_BATCH) batches.push([p, ids.slice(i, i + MAX_SESSION_BATCH)]);
    const out: Record<string, MSession[]> = {};
    for (const got of await mapLimit(batches, SESSION_CONCURRENCY, ([p, ids]) => call(p, ids)))
      for (const [id, list] of Object.entries(got)) if (list.length > 0) out[id] = list;
    return out;
  };
}

interface Page<T> {
  items: T[];
  page_info: { has_next_page: boolean; next_cursor?: string };
}

const MAX_PAGES = 200;

async function* pages<T>(list: IndexList, op: string, params: Record<string, string>): AsyncGenerator<T[]> {
  let cursor = "";
  for (let i = 0; i < MAX_PAGES; i++) {
    const p = (await list(op, { ...params, limit: "200", ...(cursor ? { cursor } : {}) })) as Page<T>;
    yield p.items;
    if (!p.page_info.has_next_page || !p.page_info.next_cursor || p.page_info.next_cursor === cursor) return;
    cursor = p.page_info.next_cursor;
  }
}

const ms = (t: string | undefined): number => (t ? Date.parse(t) : NaN);

/** Loads everything build() needs. Lists are newest first, so paging stops at the lookback. */
export async function collect(
  list: IndexList,
  factoryId: string,
  scorers: readonly Scorer[],
  sinceMs: number,
  untilMs: number,
  lookbackMs = DEFAULT_LOOKBACK_MS,
  sessions?: SessionLoader,
): Promise<MetricsData> {
  if (!factoryId) throw new OfError("invalid_request", "factory id is required");
  if (!(untilMs > sinceMs)) throw new OfError("invalid_request", "until must be after since");
  const from = sinceMs - Math.max(0, lookbackMs);
  const d = emptyData();
  d.scorers = [...scorers];

  for await (const page of pages<MWorkItem>(list, "workItems", { factory_id: factoryId })) {
    for (const w of page) if (ms(w.created_at) >= from && ms(w.created_at) < untilMs) d.workItems.push(w);
    if (page.length > 0 && ms(page[page.length - 1]!.created_at) < from) break;
  }
  for await (const page of pages<MRun>(list, "runs", { factory_id: factoryId })) {
    for (const r of page) if (ms(r.created_at) >= from && ms(r.created_at) < untilMs) d.runs.push(r);
    if (page.length > 0 && ms(page[page.length - 1]!.created_at) < from) break;
  }
  // An inbox item comes after its work item, so an item created before the
  // lookback can belong to no loaded work item, and human wait counts only
  // items of the window.
  for await (const page of pages<MInbox>(list, "inbox", {
    factory_id: factoryId,
    scope: "team",
    since_ms: String(from),
  })) {
    for (const it of page) if (ms(it.created_at) >= from) d.inbox.push(it);
    if (page.length > 0 && ms(page[page.length - 1]!.created_at) < from) break;
  }
  for await (const page of pages<MScore>(list, "scores", { factory_id: factoryId })) {
    for (const s of page) if (ms(s.created_at) >= sinceMs) d.scores.push(s);
    if (page.length > 0 && ms(page[page.length - 1]!.created_at) < sinceMs) break;
  }

  // Timelines: only for the work items that matter for the window.
  const need = new Set<string>();
  for (const w of d.workItems) {
    const c = ms(w.created_at);
    const done = w.completed_at ? ms(w.completed_at) : NaN;
    if (
      (c >= sinceMs && c < untilMs) ||
      (done >= sinceMs && done < untilMs) ||
      ((w.pull_requests?.length ?? 0) > 0 && ms(w.updated_at) >= sinceMs)
    )
      need.add(w.id);
  }
  for await (const page of pages<{ work_item_id: string; kind: string; at_ms: number; data: unknown; id: string }>(
    list,
    "metricEvents",
    { factory_id: factoryId, since_ms: String(from), sort_order: "asc" },
  )) {
    for (const e of page) {
      if (!need.has(e.work_item_id)) continue;
      (d.events[e.work_item_id] ??= []).push({
        id: e.id,
        work_item_id: e.work_item_id,
        kind: e.kind,
        data: e.data,
        created_at: new Date(e.at_ms).toISOString(),
      } as WorkItemEvent);
    }
  }

  const wanted = new Set<string>();
  for (const w of d.workItems)
    for (const pr of w.pull_requests ?? []) if (pr.repo && pr.number) wanted.add(prFactsKey(pr));
  d.facts = await loadFacts(list, wanted);
  // The sessions of the runs created in the window, for the compute time.
  if (sessions)
    d.sessions = await sessions(d.runs.filter((r) => ms(r.created_at) >= sinceMs && ms(r.created_at) < untilMs));
  return d;
}

/** How many pull requests at most loadFacts reads one by one (one list call each). */
const MAX_PR_KEY_QUERIES = MAX_PAGES;
const PR_KEY_CONCURRENCY = 8;

/** Runs fn over items with at most `limit` calls at a time; results keep the order of items. */
async function mapLimit<T, R>(items: readonly T[], limit: number, fn: (x: T) => Promise<R>): Promise<R[]> {
  const out = new Array<R>(items.length);
  let next = 0;
  const worker = async (): Promise<void> => {
    while (next < items.length) {
      const i = next++;
      out[i] = await fn(items[i]!);
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return out;
}

async function eventsOf(list: IndexList, prKey: string): Promise<FactEvent[]> {
  const out: FactEvent[] = [];
  for await (const page of pages<FactEvent>(list, "prFacts", { pr_key: prKey, sort_order: "asc" })) out.push(...page);
  return out;
}

/** IndexDO order: received time, then event ID. */
function byArrival(a: FactEvent, b: FactEvent): number {
  const d = Date.parse(a.received_at) - Date.parse(b.received_at);
  if (d !== 0 && !Number.isNaN(d)) return d;
  const x = a.id ?? "";
  const y = b.id ?? "";
  return x < y ? -1 : x > y ? 1 : 0;
}

/**
 * The facts of the wanted pull requests (lower-case PRFactsKey), from the
 * forge events in arrival order. IndexDO stores each pull request event under
 * the PRFactsKey of its pull request and each push under the key of its branch
 * (pushFactsKey). The facts of one pull request depend only on its own events
 * and on the pushes to its head branches. So for up to MAX_PR_KEY_QUERIES pull
 * requests it replays only those events, with one list call per pull request
 * and one per head branch; for more it replays all events of the team, which
 * costs at most MAX_PAGES list calls.
 */
export async function loadFacts(
  list: IndexList,
  wanted: ReadonlySet<string>,
  maxKeyQueries = MAX_PR_KEY_QUERIES,
): Promise<Record<string, PullRequestFacts>> {
  const facts: Record<string, PullRequestFacts> = {};
  const keep = (all: readonly PullRequestFacts[]): void => {
    for (const f of all) if (wanted.has(f.key.toLowerCase())) facts[f.key.toLowerCase()] = f;
  };
  const replay = (events: Iterable<FactEvent>): PullRequestFacts[] => {
    const book = new FactsBook();
    for (const e of events) book.record(e);
    return book.list();
  };
  if (wanted.size > maxKeyQueries) {
    const book = new FactsBook();
    for await (const page of pages<FactEvent>(list, "prFacts", { sort_order: "asc" }))
      for (const e of page) book.record(e);
    keep(book.list());
    return facts;
  }
  const keys = [...wanted];
  const own = await mapLimit(keys, PR_KEY_CONCURRENCY, (k) => eventsOf(list, k));
  // The head branches of each pull request, as its events name them.
  const branchKeys = own.map((evs) => {
    const ks = new Set<string>();
    for (const e of evs) {
      const repo = (e.source as { repo?: string }).repo ?? "";
      const branch = e.attributes?.["head_branch"];
      if (repo && typeof branch === "string" && branch !== "") ks.add(pushFactsKey(e.provider, repo, branch));
    }
    return ks;
  });
  const distinct = [...new Set(branchKeys.flatMap((ks) => [...ks]))];
  const pushes = new Map<string, FactEvent[]>();
  const loaded = await mapLimit(distinct, PR_KEY_CONCURRENCY, (k) => eventsOf(list, k));
  distinct.forEach((k, i) => pushes.set(k, loaded[i]!));
  own.forEach((evs, i) => {
    const extra = [...branchKeys[i]!].flatMap((k) => pushes.get(k) ?? []);
    keep(replay(extra.length ? [...evs, ...extra].sort(byArrival) : evs));
  });
  return facts;
}

/** Compute: collect and build. */
export async function compute(
  list: IndexList,
  factoryId: string,
  scorers: readonly Scorer[],
  sinceMs: number,
  untilMs: number,
  sessions?: SessionLoader,
): Promise<Dashboard> {
  const d = await collect(list, factoryId, scorers, sinceMs, untilMs, DEFAULT_LOOKBACK_MS, sessions);
  return build(factoryId, d, sinceMs, untilMs);
}
