// Transcript compaction: the messages of a finished run move from the WorkDO
// table `run_messages` to one R2 object, so a long-lived WorkDO stays small
// (plan rule 9; table `run_messages` "compaction to R2").
//
// Layout: `runs/<runId>/messages.ndjson`, one RunMessage JSON per line, in
// sequence order. The object exists once a run has been compacted. WorkDO keeps
// its counters (`runs.msg_seq`), so new messages continue the sequence.
//
// Compaction is idempotent and safe to repeat:
//   1. read the rows after the compacted prefix (chunked),
//   2. write the object (old lines + new lines) to R2,
//   3. in one transaction delete exactly the rows that the object holds.
// A crash after step 2 leaves rows and object overlapping; the reader ignores
// object lines at or below the first remaining row (see mergeMessages).
import type { RunMessage } from "@openfactory/core";

export const COMPACT_AFTER_MS = 24 * 3600_000;
/** Rows read in one step. A step that reads this many rows asks for another step. */
export const COMPACT_CHUNK_ROWS = 2000;
/** Largest object that one compaction writes (R2 allows more; this bounds memory in the DO). */
export const COMPACT_MAX_BYTES = 32 << 20;

export type CompactedMessage = Pick<RunMessage, "seq" | "role" | "content"> & Partial<RunMessage>;

export const compactedKey = (runId: string): string => `runs/${runId}/messages.ndjson`;

const dec = new TextDecoder();
const enc = new TextEncoder();

function parseLines(text: string): CompactedMessage[] {
  const out: CompactedMessage[] = [];
  for (const line of text.split("\n")) {
    if (line === "") continue;
    out.push(JSON.parse(line) as CompactedMessage);
  }
  return out;
}

/** The compacted messages of a run in sequence order. Empty when the run has not been compacted. */
export async function readCompactedMessages(bucket: R2Bucket, runId: string): Promise<CompactedMessage[]> {
  const obj = await bucket.get(compactedKey(runId));
  if (!obj) return [];
  return parseLines(dec.decode(await obj.arrayBuffer()));
}

/** The compacted prefix followed by the rows that remain, without duplicates (rows win). */
export function mergeMessages<T extends { seq: number }>(compacted: readonly T[], rows: readonly T[]): T[] {
  const firstRow = rows.length > 0 ? Math.min(...rows.map((r) => r.seq)) : Infinity;
  return [...compacted.filter((m) => m.seq < firstRow), ...rows];
}

/** What a compaction needs from the WorkDO. Reads and the delete are synchronous SQL. */
export interface CompactionHost {
  bucket: R2Bucket;
  /** Rows of run_messages after `afterSeq`, in order, at most `limit`. `data` is the message JSON. */
  readRows(runId: string, afterSeq: number, limit: number): { seq: number; data: string }[];
  /** Deletes rows up to and including `seq` in one transaction. */
  deleteRows(runId: string, uptoSeq: number): number;
  now(): number;
}

export interface CompactionResult {
  /** Rows moved to R2 by this step. */
  moved: number;
  /** True when more rows wait: schedule another step. */
  more: boolean;
  key: string;
  bytes: number;
}

/**
 * One compaction step of a run. Appends up to COMPACT_CHUNK_ROWS rows to the
 * object and deletes them. Returns `more` when the table still has rows.
 */
export async function compactRunMessages(host: CompactionHost, runId: string): Promise<CompactionResult> {
  const key = compactedKey(runId);
  const rows = host.readRows(runId, 0, COMPACT_CHUNK_ROWS);
  if (rows.length === 0) return { moved: 0, more: false, key, bytes: 0 };
  const existing = await host.bucket.get(key);
  const prev = existing ? dec.decode(await existing.arrayBuffer()) : "";
  const prevMsgs = prev === "" ? [] : parseLines(prev);
  const lastCompacted = prevMsgs.length > 0 ? prevMsgs[prevMsgs.length - 1]!.seq : 0;
  // Rows at or below the compacted prefix are leftovers of a crash after the write.
  const fresh = rows.filter((r) => r.seq > lastCompacted);
  const upto = rows[rows.length - 1]!.seq;
  let bytes = enc.encode(prev).length;
  if (fresh.length > 0) {
    // Normalize each row to one line: the stored JSON has no newline, but be sure.
    const lines = fresh.map((r) => JSON.stringify(JSON.parse(r.data)));
    const body = (prev === "" || prev.endsWith("\n") ? prev : prev + "\n") + lines.join("\n") + "\n";
    bytes = enc.encode(body).length;
    if (bytes > COMPACT_MAX_BYTES) {
      // Leave the rows in the table: the object is large enough. Nothing more to do for this run.
      return { moved: 0, more: false, key, bytes: enc.encode(prev).length };
    }
    await host.bucket.put(key, body, {
      httpMetadata: { contentType: "application/x-ndjson" },
      customMetadata: { count: String(prevMsgs.length + fresh.length), maxSeq: String(fresh[fresh.length - 1]!.seq) },
    });
    const head = await host.bucket.head(key);
    if (!head || head.size !== bytes)
      throw new Error(`compaction of ${runId}: the object ${key} does not have the expected size`);
  }
  const deleted = host.deleteRows(runId, upto);
  return { moved: deleted, more: rows.length >= COMPACT_CHUNK_ROWS, key, bytes };
}

/** When a finished run should be compacted. */
export const compactionDueMs = (finishedMs: number): number => finishedMs + COMPACT_AFTER_MS;

/** Methods a DO exposes for the compaction queue handler. */
export interface CompactionRpc {
  compactRun(runId: string): Promise<{ more: boolean }>;
}
