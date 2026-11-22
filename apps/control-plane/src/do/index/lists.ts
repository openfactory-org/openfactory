// The paged reads of the IndexDO read model, one ListSpec per list. Each spec
// names its table, its filters and how a row becomes an item; runList builds
// the WHERE clause and runs one keyset page (lib/page sqlPage).
import { OfError } from "@openfactory/core";
import { listResponse, pageFrom, sqlPage, type ListResponse } from "../../lib/page";
import type { J } from "./projections";

type Query = Record<string, string>;
type Row = Record<string, SqlStorageValue>;

/** Extra conditions of a list, joined with AND. */
interface Filter {
  where: string[];
  args: unknown[];
}

interface ListSpec {
  /** "SELECT ... FROM ..." without WHERE, ORDER or LIMIT; a function when it binds query values. */
  from: string | ((q: Query) => { select: string; args: unknown[] });
  /** Time column of the keyset (default created_ms). */
  timeCol?: string;
  /** Query key to column, matched with "=". */
  eq?: Record<string, string>;
  /** Query key to column, matched with IN over a comma list. */
  anyOf?: Record<string, string>;
  /** Column that since_ms (unix ms, inclusive) filters; null when the list has no since_ms. */
  since?: string | null;
  /** Fixed conditions. */
  where?: string[];
  /** Conditions that depend on the query in other ways. */
  filter?: (q: Query, f: Filter) => void;
  /** Items of the page rows (default: the parsed data column). */
  items?: (sql: SqlStorage, rows: Row[], q: Query) => unknown[];
}

const data = (rows: Row[]): unknown[] => rows.map((r) => JSON.parse(String(r["data"])) as unknown);

/** Bound parameters per query stay under the SQLite limit of Durable Objects (100). */
export const IN_CHUNK = 90;

/** Recipients of the inbox items, each list sorted by user ID: one query per IN_CHUNK items. */
function recipientsOf(sql: SqlStorage, ids: string[]): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (let i = 0; i < ids.length; i += IN_CHUNK) {
    const chunk = ids.slice(i, i + IN_CHUNK);
    const rows = sql
      .exec<{ inbox_id: string; user_id: string }>(
        `SELECT inbox_id, user_id FROM inbox_recipients WHERE inbox_id IN (${chunk.map(() => "?").join(",")})
         ORDER BY inbox_id, user_id`,
        ...chunk,
      )
      .toArray();
    for (const r of rows) {
      const list = out.get(r.inbox_id);
      if (list) list.push(r.user_id);
      else out.set(r.inbox_id, [r.user_id]);
    }
  }
  return out;
}

/**
 * Inbox items as InboxItemView (api/inbox.go): the read state of the caller
 * comes from a LEFT JOIN on inbox_reads; the recipients of items whose data
 * has none come from one chunked query.
 */
function inboxItems(sql: SqlStorage, rows: Row[]): unknown[] {
  const items = rows.map((row) => {
    const it = JSON.parse(String(row["data"])) as J;
    it["is_read"] = row["is_read"] === 1;
    return it;
  });
  const missing = rows.filter((_, i) => !Array.isArray(items[i]!["recipients"])).map((r) => String(r["id"]));
  if (missing.length === 0) return items;
  const recipients = recipientsOf(sql, missing);
  items.forEach((it, i) => {
    if (!Array.isArray(it["recipients"])) it["recipients"] = recipients.get(String(rows[i]!["id"])) ?? [];
  });
  return items;
}

/**
 * The lists of IndexDO.list. Query keys:
 *   runs          factory_id, work_item_id, parent_run_id, root_run_id, creator, state (comma list), source, model, since_ms
 *   workItems     factory_id, stage (comma list), since_ms
 *   inbox         user_id (recipient or assignee; scope=team for all), factory_id, work_item_id,
 *                 resolved ("true"/"false"), unread ("true", with user_id), since_ms
 *   scores        factory_id, scorer_id, run_id, state (comma list), since_ms
 *   benchmarkRuns factory_id, suite, state (comma list), since_ms
 *   deadLetters
 *   prFacts       pr_key, repo, head_branch
 *   metricEvents  factory_id or work_item_id, since_ms
 */
export const LISTS: Readonly<Record<string, ListSpec>> = {
  runs: {
    from: "SELECT id, created_ms, data FROM run_summaries",
    eq: {
      factory_id: "factory_id",
      work_item_id: "work_item_id",
      parent_run_id: "parent_run_id",
      root_run_id: "root_run_id",
      creator: "creator_id",
      source: "source",
      model: "model",
    },
    anyOf: { state: "state" },
  },
  workItems: {
    from: "SELECT id, created_ms, data FROM work_item_summaries",
    eq: { factory_id: "factory_id" },
    anyOf: { stage: "stage" },
  },
  inbox: {
    from: (q) =>
      q["user_id"]
        ? {
            select: `SELECT inbox.id AS id, inbox.created_ms AS created_ms, inbox.data AS data,
                       r.inbox_id IS NOT NULL AS is_read
                     FROM inbox LEFT JOIN inbox_reads r ON r.inbox_id = inbox.id AND r.user_id = ?`,
            args: [q["user_id"]],
          }
        : { select: "SELECT id, created_ms, data, 0 AS is_read FROM inbox", args: [] },
    eq: { factory_id: "factory_id", work_item_id: "work_item_id" },
    filter: (q, f) => {
      if (q["resolved"] === "true" || q["resolved"] === "false") {
        f.where.push("resolved = ?");
        f.args.push(q["resolved"] === "true" ? 1 : 0);
      }
      const user = q["user_id"] ?? "";
      if (user && q["scope"] !== "team") {
        f.where.push("(assignee_id = ? OR id IN (SELECT inbox_id FROM inbox_recipients WHERE user_id = ?))");
        f.args.push(user, user);
      }
      if (user && q["unread"] === "true") f.where.push("r.inbox_id IS NULL");
    },
    items: inboxItems,
  },
  scores: {
    from: "SELECT id, created_ms, data FROM scores",
    eq: { factory_id: "factory_id", scorer_id: "scorer_id", run_id: "run_id" },
    anyOf: { state: "state" },
  },
  benchmarkRuns: {
    from: "SELECT id, created_ms, data FROM benchmark_summaries",
    eq: { factory_id: "factory_id", suite: "suite" },
    anyOf: { state: "state" },
  },
  deadLetters: {
    from: "SELECT id, origin_do, origin_msg_id, kind, lane, attempts, last_error, created_ms FROM dead_letters",
    where: ["redriven = 0"],
    since: null,
    items: (_sql, rows) => rows,
  },
  prFacts: {
    from: "SELECT id, received_ms, data FROM pull_request_facts",
    timeCol: "received_ms",
    eq: { pr_key: "pr_key", repo: "repo", head_branch: "head_branch" },
    since: null,
  },
  metricEvents: {
    from: "SELECT id, work_item_id, kind, at_ms, data FROM work_item_metric_events",
    timeCol: "at_ms",
    eq: { factory_id: "factory_id", work_item_id: "work_item_id" },
    since: "at_ms",
    items: (_sql, rows) =>
      rows.map((x) => ({
        id: x["id"],
        work_item_id: x["work_item_id"],
        kind: x["kind"],
        at_ms: x["at_ms"],
        data: JSON.parse(String(x["data"])) as unknown,
      })),
  },
};

/** One page of a list. Paging: limit, cursor, sort_order. */
export function runList(sql: SqlStorage, op: string, q: Query): ListResponse<unknown> {
  const spec = Object.hasOwn(LISTS, op) ? LISTS[op] : undefined;
  if (!spec) throw new OfError("invalid_request", `unknown list ${op}`);
  const page = pageFrom(q);
  const from = typeof spec.from === "string" ? { select: spec.from, args: [] } : spec.from(q);
  const f: Filter = { where: [...(spec.where ?? [])], args: [] };
  for (const [key, col] of Object.entries(spec.eq ?? {})) {
    if (q[key]) {
      f.where.push(`${col} = ?`);
      f.args.push(q[key]);
    }
  }
  for (const [key, col] of Object.entries(spec.anyOf ?? {})) {
    const vals = (q[key] ?? "")
      .split(",")
      .map((x) => x.trim())
      .filter(Boolean);
    if (vals.length) {
      f.where.push(`${col} IN (${vals.map(() => "?").join(",")})`);
      f.args.push(...vals);
    }
  }
  spec.filter?.(q, f);
  const sinceCol = spec.since === undefined ? "created_ms" : spec.since;
  const since = Number(q["since_ms"] ?? "");
  if (sinceCol && q["since_ms"] && Number.isFinite(since)) {
    f.where.push(`${sinceCol} >= ?`);
    f.args.push(since);
  }
  // The select's own placeholders come before the WHERE clause.
  const r = sqlPage(
    sql,
    {
      select: from.select,
      where: f.where,
      args: [...from.args, ...f.args],
      ...(spec.timeCol ? { timeCol: spec.timeCol } : {}),
    },
    page,
  );
  return listResponse(spec.items ? spec.items(sql, r.rows, q) : data(r.rows), r.next);
}
