// IndexDO lists: the inbox page reads the read state and the recipients in a
// fixed number of queries, and the list registry keeps rows and order.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { newToken } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { IndexDO } from "./index";
import { IN_CHUNK, runList } from "./lists";

type Item = { id: string; is_read: boolean; recipients: string[]; title?: string };

function fresh(): IndexDO {
  return env.INDEX.getByName(`team:x${newToken("w")}|index`) as unknown as IndexDO;
}

/** Runs fn with the DO SQL and counts the queries it runs. */
function counted<T>(stub: IndexDO, fn: (sql: SqlStorage) => T): Promise<{ out: T; queries: number }> {
  return runInDurableObject(stub as unknown as DurableObjectStub, (_instance, state) => {
    const sql = state.storage.sql;
    let queries = 0;
    const exec = sql.exec.bind(sql);
    const spy = ((query: string, ...args: unknown[]) => {
      queries++;
      return exec(query, ...(args as SqlStorageValue[]));
    }) as SqlStorage["exec"];
    Object.defineProperty(sql, "exec", { value: spy, configurable: true, writable: true });
    try {
      return { out: fn(sql), queries };
    } finally {
      delete (sql as { exec?: unknown }).exec;
    }
  });
}

/**
 * Inserts n inbox items created at 1000+i ms. Items with an even index keep
 * their recipients in data; odd ones only in inbox_recipients (two users, in
 * reverse order). Items whose index is a multiple of 3 are read by u1.
 */
function seed(sql: SqlStorage, n: number): void {
  for (let i = 0; i < n; i++) {
    const id = `inb_${String(i).padStart(4, "0")}`;
    const data: Record<string, unknown> = { id, title: `t${i}` };
    if (i % 2 === 0) data["recipients"] = ["u1"];
    sql.exec(
      "INSERT INTO inbox (id, owner, factory_id, created_ms, version, data) VALUES (?, 'work', 'fac_a', ?, 1, ?)",
      id,
      1000 + i,
      JSON.stringify(data),
    );
    if (i % 2 === 1) {
      sql.exec("INSERT INTO inbox_recipients (inbox_id, user_id) VALUES (?, 'u2')", id);
      sql.exec("INSERT INTO inbox_recipients (inbox_id, user_id) VALUES (?, 'u1')", id);
    } else sql.exec("INSERT INTO inbox_recipients (inbox_id, user_id) VALUES (?, 'u1')", id);
    if (i % 3 === 0) sql.exec("INSERT INTO inbox_reads (inbox_id, user_id, read_ms) VALUES (?, 'u1', 1)", id);
  }
}

/** The view the old per-row code built for item i of seed. */
function expected(i: number, user: string): Item {
  return {
    id: `inb_${String(i).padStart(4, "0")}`,
    title: `t${i}`,
    recipients: i % 2 === 0 ? ["u1"] : ["u1", "u2"],
    is_read: user === "u1" && i % 3 === 0,
  };
}

describe("IndexDO inbox list", () => {
  it("reads one page of 200 items in 1 + ceil(missing / IN_CHUNK) queries", async () => {
    const stub = fresh();
    await stub.ping();
    await runInDurableObject(stub as unknown as DurableObjectStub, (_i, state) => seed(state.storage.sql, 250));
    const { out, queries } = await counted(stub, (sql) =>
      runList(sql, "inbox", { user_id: "u1", factory_id: "fac_a", limit: "200" }),
    );
    // The old code ran one read-state query per item and one recipients query per
    // item without recipients in data: 1 + 200 + 100 = 301 queries for this page.
    expect(queries).toBe(1 + Math.ceil(100 / IN_CHUNK));
    const items = out.items as Item[];
    expect(items).toHaveLength(200);
    expect(items).toEqual(Array.from({ length: 200 }, (_, k) => expected(249 - k, "u1")));
    expect(out.page_info.has_next_page).toBe(true);
  });

  it("keeps rows and order for the filters, the cursor and a caller without a user", async () => {
    const stub = fresh();
    await stub.ping();
    await runInDurableObject(stub as unknown as DurableObjectStub, (_i, state) => seed(state.storage.sql, 12));
    const ids = (items: unknown[]) => (items as Item[]).map((x) => x.id);
    const list = (q: Record<string, string>) =>
      runInDurableObject(stub as unknown as DurableObjectStub, (_i, state) => runList(state.storage.sql, "inbox", q));

    const unread = await list({ user_id: "u1", unread: "true", sort_order: "asc" });
    expect(ids(unread.items)).toEqual([1, 2, 4, 5, 7, 8, 10, 11].map((i) => expected(i, "u1").id));
    expect((unread.items as Item[]).every((x) => !x.is_read)).toBe(true);

    const mine = await list({ user_id: "u2" });
    expect(mine.items).toEqual([11, 9, 7, 5, 3, 1].map((i) => expected(i, "u2")));

    const team = await list({ user_id: "u2", scope: "team", limit: "5" });
    expect(ids(team.items)).toEqual([11, 10, 9, 8, 7].map((i) => expected(i, "").id));
    const next = await list({ user_id: "u2", scope: "team", limit: "5", cursor: team.page_info.next_cursor! });
    expect(ids(next.items)).toEqual([6, 5, 4, 3, 2].map((i) => expected(i, "").id));

    const anon = await list({ since_ms: "1010" });
    expect(anon.items).toEqual([11, 10].map((i) => expected(i, "")));
  });

  it("rejects an unknown list", async () => {
    const stub = fresh();
    await stub.ping();
    await expect(
      runInDurableObject(stub as unknown as DurableObjectStub, (_i, state) =>
        runList(state.storage.sql, "toString", {}),
      ),
    ).rejects.toThrow(/unknown list toString/);
  });
});
