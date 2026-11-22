// Collects the inputs of build() from the read model (IndexDO lists) and the
// scorers (TeamDO). Port of Collect/Compute in internal/metrics.
import { OfError, type PullRequestFacts, type Scorer, type WorkItemEvent } from "@openfactory/core";
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
} from "./build";
import { FactsBook, type FactEvent } from "./facts";

/** IndexRpc.list(meta, op, params) with the meta bound. */
export type IndexList = (op: string, params: Record<string, string>) => Promise<unknown>;

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
  for await (const page of pages<MInbox>(list, "inbox", { factory_id: factoryId, scope: "team" }))
    d.inbox.push(...page);
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

  // Pull request facts: replay the forge events in arrival order.
  const book = new FactsBook();
  for await (const page of pages<FactEvent>(list, "prFacts", { sort_order: "asc" }))
    for (const e of page) book.record(e);
  const wanted = new Set<string>();
  for (const w of d.workItems)
    for (const pr of w.pull_requests ?? []) if (pr.repo && pr.number) wanted.add(prFactsKey(pr));
  const facts: Record<string, PullRequestFacts> = {};
  for (const f of book.list()) if (wanted.has(f.key.toLowerCase())) facts[f.key.toLowerCase()] = f;
  d.facts = facts;
  return d;
}

/** Compute: collect and build. */
export async function compute(
  list: IndexList,
  factoryId: string,
  scorers: readonly Scorer[],
  sinceMs: number,
  untilMs: number,
): Promise<Dashboard> {
  return build(factoryId, await collect(list, factoryId, scorers, sinceMs, untilMs), sinceMs, untilMs);
}
