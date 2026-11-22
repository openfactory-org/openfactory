import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import {
  COMPACT_CHUNK_ROWS,
  compactRunMessages,
  compactedKey,
  mergeMessages,
  readCompactedMessages,
  type CompactionHost,
} from "./compact";
import { deletePrefix, parseRetention, pruneAttachments, pruneRunData, runPrefixes } from "../retention/sweep";

function host(rows: { seq: number; data: string }[]): CompactionHost & { rows: typeof rows } {
  return {
    rows,
    bucket: env.DATA,
    readRows: (_r, after, limit) => rows.filter((x) => x.seq > after).slice(0, limit),
    deleteRows: (_r, upto) => {
      const n = rows.filter((x) => x.seq <= upto).length;
      for (let i = rows.length - 1; i >= 0; i--) if (rows[i]!.seq <= upto) rows.splice(i, 1);
      return n;
    },
    now: Date.now,
  };
}
const msg = (seq: number) => ({ seq, data: JSON.stringify({ seq, role: "assistant", content: `m${seq}` }) });

describe("compaction", () => {
  it("moves messages to R2, reads them back and appends later steps", async () => {
    const runId = "run_c1" + crypto.randomUUID();
    const h = host([msg(1), msg(2), msg(3)]);
    const r = await compactRunMessages(h, runId);
    expect(r).toMatchObject({ moved: 3, more: false });
    expect(h.rows).toHaveLength(0);
    expect((await readCompactedMessages(env.DATA, runId)).map((m) => m.content)).toEqual(["m1", "m2", "m3"]);
    h.rows.push(msg(4), msg(5));
    await compactRunMessages(h, runId);
    expect((await readCompactedMessages(env.DATA, runId)).map((m) => m.seq)).toEqual([1, 2, 3, 4, 5]);
    expect(await compactRunMessages(h, runId)).toMatchObject({ moved: 0, more: false });
  });
  it("is safe after a crash between the write and the delete, and works in chunks", async () => {
    const runId = "run_c2" + crypto.randomUUID();
    const rows = Array.from({ length: COMPACT_CHUNK_ROWS + 5 }, (_, i) => msg(i + 1));
    const h = host(rows);
    const first = await compactRunMessages(
      {
        ...h,
        deleteRows: () => {
          throw new Error("crash");
        },
      },
      runId,
    ).catch((e: Error) => e);
    expect(first).toBeInstanceOf(Error);
    expect(rows).toHaveLength(COMPACT_CHUNK_ROWS + 5);
    const a = await compactRunMessages(h, runId);
    expect(a.more).toBe(true);
    const b = await compactRunMessages(h, runId);
    expect(b.more).toBe(false);
    const all = await readCompactedMessages(env.DATA, runId);
    expect(all).toHaveLength(COMPACT_CHUNK_ROWS + 5);
    expect(new Set(all.map((m) => m.seq)).size).toBe(all.length);
    expect(await env.DATA.head(compactedKey(runId))).toBeTruthy();
  });
  it("merges the compacted prefix with remaining rows without duplicates", () => {
    expect(mergeMessages([{ seq: 1 }, { seq: 2 }, { seq: 3 }], [{ seq: 3 }, { seq: 4 }]).map((m) => m.seq)).toEqual([
      1, 2, 3, 4,
    ]);
  });
});

describe("retention", () => {
  it("parses durations", () => {
    expect(parseRetention("")).toBe(0);
    expect(parseRetention("0")).toBe(0);
    expect(parseRetention("720h")).toBe(720 * 3600_000);
    expect(parseRetention("1h30m")).toBe(5400_000);
    expect(parseRetention("30d")).toBe(30 * 86_400_000);
    expect(() => parseRetention("soon")).toThrow();
  });
  it("deletes a prefix and prunes only expired runs", async () => {
    const t = "t" + crypto.randomUUID();
    const mk = async (id: string) => {
      for (const p of runPrefixes(t, id)) await env.DATA.put(p + "a", "x");
    };
    await mk("old");
    await mk("new");
    const now = Date.now();
    const out = await pruneRunData({
      bucket: env.DATA,
      teamId: t,
      runs: [
        { id: "old", lastActivityMs: now - 10_000 },
        { id: "new", lastActivityMs: now },
      ],
      nowMs: now,
      retentionMs: 5000,
    });
    expect(out).toEqual({ pruned: ["old"], more: false });
    expect((await env.DATA.list({ prefix: `runs/old/` })).objects).toHaveLength(0);
    expect((await env.DATA.list({ prefix: `snapshots/${t}/old/` })).objects).toHaveLength(0);
    expect((await env.DATA.list({ prefix: `runs/new/` })).objects).toHaveLength(1);
    expect(
      await pruneRunData({
        bucket: env.DATA,
        teamId: t,
        runs: [{ id: "new", lastActivityMs: 0 }],
        nowMs: now,
        retentionMs: 0,
      }),
    ).toEqual({ pruned: [], more: false });
    for (let i = 0; i < 3; i++) await env.DATA.put(`scratch/${t}/${i}`, "x");
    expect((await deletePrefix(env.DATA, `scratch/${t}/`, 2)).done).toBe(false);
    expect((await deletePrefix(env.DATA, `scratch/${t}/`, 2)).done).toBe(true);
  });
  it("prunes attachments by upload age", async () => {
    const id = crypto.randomUUID();
    await env.DATA.put(`attachments/team/${id}/a.txt`, "x");
    expect((await pruneAttachments(env.DATA, Date.now(), 3_600_000)).ids).not.toContain(id);
    const out = await pruneAttachments(env.DATA, Date.now() + 7_200_000, 3_600_000);
    expect(out.ids).toContain(id);
    expect(await env.DATA.head(`attachments/team/${id}/a.txt`)).toBeNull();
  });
});
