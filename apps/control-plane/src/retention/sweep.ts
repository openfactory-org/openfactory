// Retention sweeps. Port of the retention loops of internal/server and
// internal/objectstore (RunData.Prune, PruneAttachments), cut into steps that
// fit one alarm or one Queue message (a Worker invocation has a subrequest
// limit, and a failing alarm is retried only 6 times).
//
// - Run data: the objects of a finished run (`runs/<id>/`, `snapshots/<team>/<id>/`)
//   are deleted when the run's last activity is older than
//   OF_OBJECT_STORE_RETENTION. The WorkDO knows its runs; it calls pruneRunData.
// - Attachments: uploaded files `attachments/<team>/<id>/<name>` by age of the
//   upload (pruneAttachments). The R2 lifecycle rule on `attachments/` does the
//   same without code; this is the fallback when no rule is set.
// - IndexDO: the read model has bounded tables (pruneIndex).
// - TeamDO: the metadata rows of attachments (pruneAttachmentRows).

/** R2 delete accepts at most 1000 keys. */
export const R2_DELETE_BATCH = 1000;
/** Keys deleted in one step. */
export const DEFAULT_STEP_BUDGET = 5000;

/**
 * Parses OF_OBJECT_STORE_RETENTION (a Go duration such as "720h", or "30d").
 * "" and "0" keep forever (returns 0). Throws on a malformed value.
 */
export function parseRetention(v: string | undefined): number {
  const s = (v ?? "").trim();
  if (s === "" || s === "0") return 0;
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h|d)/g;
  let total = 0;
  let pos = 0;
  for (let m = re.exec(s); m !== null; m = re.exec(s)) {
    if (m.index !== pos)
      throw new Error(`OF_OBJECT_STORE_RETENTION ${JSON.stringify(v)}: want a duration such as 720h (0 keeps forever)`);
    pos = re.lastIndex;
    const n = Number(m[1]);
    const unit = { ns: 1e-6, us: 1e-3, µs: 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000, d: 86_400_000 }[m[2] as "ms"];
    total += n * unit;
  }
  if (pos !== s.length || total < 0)
    throw new Error(`OF_OBJECT_STORE_RETENTION ${JSON.stringify(v)}: want a duration such as 720h (0 keeps forever)`);
  return Math.round(total);
}

/** The retention period of the environment in ms; 0 keeps forever. */
export function retentionMs(env: { OF_OBJECT_STORE_RETENTION?: string }): number {
  return parseRetention(env.OF_OBJECT_STORE_RETENTION);
}

export interface PrefixDeleteResult {
  deleted: number;
  /** True when the prefix is empty. */
  done: boolean;
  /** Continue from here when not done (the listing is empty after deletes, so usually unused). */
  cursor?: string;
}

/**
 * Deletes the objects under a prefix, at most `budget` in one call. Deletes are
 * idempotent, so a repeated call is safe. Call again until `done`.
 */
export async function deletePrefix(
  bucket: R2Bucket,
  prefix: string,
  budget = DEFAULT_STEP_BUDGET,
): Promise<PrefixDeleteResult> {
  let deleted = 0;
  while (deleted < budget) {
    const page = await bucket.list({ prefix, limit: Math.min(R2_DELETE_BATCH, budget - deleted) });
    if (page.objects.length === 0) return { deleted, done: true };
    await bucket.delete(page.objects.map((o) => o.key));
    deleted += page.objects.length;
    if (!page.truncated && page.objects.length < R2_DELETE_BATCH) {
      // The listing held less than a batch: after the delete the prefix is empty.
      return { deleted, done: true };
    }
  }
  return { deleted, done: false };
}

/** Keys of the data of one run. */
export function runPrefixes(teamId: string, runId: string): string[] {
  return [`runs/${runId}/`, `snapshots/${teamId}/${runId}/`];
}

export interface PruneRunsInput {
  bucket: R2Bucket;
  teamId: string;
  /** Finished runs of the caller, with the time of their last activity. */
  runs: readonly { id: string; lastActivityMs: number }[];
  nowMs: number;
  /** 0 keeps forever: nothing is pruned. */
  retentionMs: number;
  budget?: number;
}

export interface PruneRunsResult {
  /** Runs whose data is gone. */
  pruned: string[];
  /** True when the budget ended before every expired run was done: call again. */
  more: boolean;
}

/** Deletes the run data of every run older than the retention period. Runs that stay (not old enough) are untouched. */
export async function pruneRunData(input: PruneRunsInput): Promise<PruneRunsResult> {
  if (input.retentionMs <= 0) return { pruned: [], more: false };
  const cutoff = input.nowMs - input.retentionMs;
  let budget = input.budget ?? DEFAULT_STEP_BUDGET;
  const pruned: string[] = [];
  for (const r of [...input.runs].sort((a, b) => a.lastActivityMs - b.lastActivityMs)) {
    if (r.lastActivityMs >= cutoff) continue;
    let done = true;
    for (const p of runPrefixes(input.teamId, r.id)) {
      if (budget <= 0) {
        done = false;
        break;
      }
      const res = await deletePrefix(input.bucket, p, budget);
      budget -= res.deleted;
      if (!res.done) done = false;
    }
    if (!done) return { pruned, more: true };
    pruned.push(r.id);
  }
  return { pruned, more: false };
}

export const ATTACHMENTS_PREFIX = "attachments/";

export interface PruneAttachmentsResult {
  /** Attachment IDs whose objects are all deleted. */
  ids: string[];
  /** Resume here when `more`. */
  cursor?: string;
  more: boolean;
}

/**
 * Deletes uploaded attachments (`attachments/<team>/<id>/<name>`) older than
 * `olderThanMs` by the time of upload, one list page per call.
 */
export async function pruneAttachments(
  bucket: R2Bucket,
  nowMs: number,
  olderThanMs: number,
  cursor?: string,
  pageSize = 1000,
): Promise<PruneAttachmentsResult> {
  if (olderThanMs <= 0) return { ids: [], more: false };
  const cutoff = nowMs - olderThanMs;
  const page = await bucket.list({ prefix: ATTACHMENTS_PREFIX, limit: pageSize, ...(cursor ? { cursor } : {}) });
  const expired: string[] = [];
  const ids = new Set<string>();
  for (const o of page.objects) {
    const parts = o.key.slice(ATTACHMENTS_PREFIX.length).split("/");
    if (parts.length < 3 || !parts[1] || o.uploaded.getTime() >= cutoff) continue;
    expired.push(o.key);
    ids.add(parts[1]);
  }
  for (let i = 0; i < expired.length; i += R2_DELETE_BATCH) await bucket.delete(expired.slice(i, i + R2_DELETE_BATCH));
  const out: PruneAttachmentsResult = { ids: [...ids].sort(), more: page.truncated };
  if (page.truncated && page.cursor) out.cursor = page.cursor;
  return out;
}

// ---------- SQL tables ----------

const ONE_DAY = 86_400_000;

export interface IndexRetention {
  /** Work-item metric events and pull request fact events. Metrics look back 90 days; the default keeps 400. */
  eventsDays: number;
  /** Resolved inbox items. */
  inboxDays: number;
  /** Redriven and old dead letters. */
  deadLetterDays: number;
  /** Rows deleted per call. */
  chunk: number;
}

export const DEFAULT_INDEX_RETENTION: IndexRetention = {
  eventsDays: 400,
  inboxDays: 400,
  deadLetterDays: 30,
  chunk: 1000,
};

/** Deletes up to `chunk` rows with a rowid subquery (SQLite has no DELETE ... LIMIT in DO SQL). Returns the rows deleted. */
function deleteChunk(sql: SqlStorage, table: string, where: string, args: unknown[], chunk: number): number {
  return sql.exec(
    `DELETE FROM ${table} WHERE rowid IN (SELECT rowid FROM ${table} WHERE ${where} LIMIT ?)`,
    ...args,
    chunk,
  ).rowsWritten;
}

export interface SqlPruneResult {
  deleted: Record<string, number>;
  /** True when a table hit the chunk limit: call again. */
  more: boolean;
}

/** One bounded step of IndexDO retention. Call inside one transaction. */
export function pruneIndex(
  sql: SqlStorage,
  nowMs: number,
  policy: IndexRetention = DEFAULT_INDEX_RETENTION,
): SqlPruneResult {
  const deleted: Record<string, number> = {};
  const run = (name: string, table: string, where: string, args: unknown[]): void => {
    const n = deleteChunk(sql, table, where, args, policy.chunk);
    if (n > 0) deleted[name] = n;
  };
  const ev = nowMs - policy.eventsDays * ONE_DAY;
  run("work_item_metric_events", "work_item_metric_events", "at_ms < ?", [ev]);
  run("pull_request_facts", "pull_request_facts", "received_ms > 0 AND received_ms < ?", [ev]);
  run("inbox", "inbox", "resolved = 1 AND COALESCE(resolved_ms, created_ms) < ?", [nowMs - policy.inboxDays * ONE_DAY]);
  const dl = nowMs - policy.deadLetterDays * ONE_DAY;
  run("dead_letters", "dead_letters", "redriven = 1 OR created_ms < ?", [dl]);
  sql.exec("DELETE FROM inbox_recipients WHERE inbox_id NOT IN (SELECT id FROM inbox)");
  sql.exec("DELETE FROM inbox_reads WHERE inbox_id NOT IN (SELECT id FROM inbox)");
  return { deleted, more: Object.values(deleted).some((n) => n >= policy.chunk) };
}

/** TeamDO: the metadata rows of attachments, by the time of upload. Returns the rows deleted. */
export function pruneAttachmentRows(
  sql: SqlStorage,
  nowMs: number,
  olderThanMs: number,
  chunk = 1000,
): { deleted: number; more: boolean } {
  if (olderThanMs <= 0) return { deleted: 0, more: false };
  const n = deleteChunk(sql, "attachments", "created_ms < ?", [nowMs - olderThanMs], chunk);
  return { deleted: n, more: n >= chunk };
}

/** Methods a DO exposes for the retention queue handler. */
export interface RetentionRpc {
  retentionSweep(): Promise<{ more: boolean }>;
}
