import { env } from "cloudflare:workers";
import { runDurableObjectAlarm, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { OutboxEnvelope } from "../contracts/do-messages";
import { insertMany } from "./sql/chunk";
import { applyMigrations, schemaSnapshot } from "./sql/migrate";
import { Outbox, OUTBOX_MAX_ATTEMPTS, outboxBackoff, type OutboxRow } from "./outbox/outbox";
import { Timers } from "./timers/timers";
import { parseDest, dest, teamOfDoName } from "./outbox/dest";

let n = 0;

/** Waits until check() passes; alarms set in the past fire on their own. */
async function eventually(check: () => Promise<void> | void, ms = 2000): Promise<void> {
  const end = Date.now() + ms;
  for (;;) {
    try {
      await check();
      return;
    } catch (e) {
      if (Date.now() > end) throw e;
      await new Promise((r) => setTimeout(r, 20));
    }
  }
}
const fresh = () => env.TEAM.getByName(`team:libtest${++n}`);

describe("sql", () => {
  it("applies migrations once and records them", async () => {
    await runInDurableObject(fresh(), async (_i, state) => {
      const list = [
        { version: 1, name: "a", sql: "CREATE TABLE x1 (id TEXT PRIMARY KEY)" },
        { version: 2, name: "b", sql: "CREATE TABLE x2 (id TEXT); CREATE INDEX x2_id ON x2 (id)" },
      ];
      expect(await applyMigrations(state.storage, "t", list)).toEqual([1, 2]);
      expect(await applyMigrations(state.storage, "t", list)).toEqual([]);
      expect(await applyMigrations(state.storage, "t", [...list, { version: 3, name: "c", sql: "CREATE TABLE x3 (k INTEGER)" }])).toEqual([3]);
      expect(schemaSnapshot(state.storage.sql)).toContain("CREATE TABLE x3");
      await expect(applyMigrations(state.storage, "t", [list[1]!, list[0]!])).rejects.toThrow(/increase/);
    });
  });

  it("rolls back a failed migration", async () => {
    await runInDurableObject(fresh(), async (_i, state) => {
      await expect(
        applyMigrations(state.storage, "bad", [{ version: 1, name: "x", sql: "CREATE TABLE y1 (a); SELECT nope FROM nothing" }]),
      ).rejects.toThrow();
      const t = state.storage.sql.exec("SELECT name FROM sqlite_master WHERE name = 'y1'").toArray();
      expect(t).toHaveLength(0);
    });
  });

  it("chunks inserts under the parameter limit", async () => {
    await runInDurableObject(fresh(), async (_i, state) => {
      const sql = state.storage.sql;
      sql.exec("CREATE TABLE big (a TEXT, b INTEGER, c TEXT)");
      const rows = Array.from({ length: 1000 }, (_, i) => [`k${i}`, i, "x".repeat(50)]);
      const statements = state.storage.transactionSync(() => insertMany(sql, "big", ["a", "b", "c"], rows));
      expect(statements).toBe(Math.ceil(1000 / 33));
      expect(sql.exec("SELECT COUNT(*) AS n FROM big").one().n).toBe(1000);
      insertMany(sql, "big", ["a", "b", "c"], [["k1", 9, "y"]], { verb: "INSERT OR IGNORE" });
      expect(() => insertMany(sql, "big; DROP", ["a"], [["x"]])).toThrow();
    });
  });
});

describe("outbox", () => {
  const mk = (state: DurableObjectState, deliver: (e: OutboxEnvelope, r: OutboxRow) => Promise<unknown>, dead: OutboxRow[] = []) => {
    let now = 1_000_000;
    const ob = new Outbox({
      storage: state.storage,
      origin: () => "team:test",
      deliver: deliver as never,
      onDead: (r) => dead.push(r),
      now: () => now,
      random: () => 0.5,
    });
    return { ob, advance: (ms: number) => (now += ms), at: () => now };
  };

  it("delivers rows of a lane in order and dedupes msgId at enqueue", async () => {
    await runInDurableObject(fresh(), async (_i, state) => {
      const got: string[] = [];
      const { ob } = mk(state, async (e) => {
        got.push(e.msgId);
      });
      state.storage.transactionSync(() => {
        ob.enqueue({ dest: "custom:x", kind: "index.project", msgId: "a", lane: "L", payload: { entity: "run", id: "1", version: 1, summary: null } });
        ob.enqueue({ dest: "custom:x", kind: "index.project", msgId: "b", lane: "L", payload: { entity: "run", id: "1", version: 2, summary: null } });
        ob.enqueue({ dest: "custom:x", kind: "index.project", msgId: "a", lane: "L", payload: { entity: "run", id: "1", version: 9, summary: null } });
      });
      expect(ob.pendingCount()).toBe(2);
      await ob.drain();
      expect(got).toEqual(["a"]);
      await ob.drain();
      expect(got).toEqual(["a", "b"]);
      expect(ob.pendingCount()).toBe(0);
    });
  });

  it("backs off, marks dead after the last attempt, and dead rows do not block the lane", async () => {
    await runInDurableObject(fresh(), async (_i, state) => {
      const dead: OutboxRow[] = [];
      const ok: string[] = [];
      const t = mk(
        state,
        async (e) => {
          if (e.msgId === "bad") throw new Error("boom");
          ok.push(e.msgId);
        },
        dead,
      );
      t.ob.enqueue({ dest: "custom:x", kind: "k", msgId: "bad", lane: "L", payload: {} });
      t.ob.enqueue({ dest: "custom:x", kind: "k", msgId: "next", lane: "L", payload: {} });
      for (let i = 0; i < OUTBOX_MAX_ATTEMPTS; i++) {
        await t.ob.drain();
        const row = t.ob.get("bad")!;
        if (row.state === "dead") break;
        expect(row.next_ms - t.at()).toBe(outboxBackoff(row.attempts, () => 0.5));
        expect(row.last_error).toBe("boom");
        t.advance(10 * 60_000);
      }
      expect(dead).toHaveLength(1);
      expect(t.ob.get("bad")!.state).toBe("dead");
      expect(ok).toEqual([]);
      await t.ob.drain();
      expect(ok).toEqual(["next"]);
      expect(t.ob.redrive("bad")).toBe(true);
      expect(t.ob.get("bad")!.attempts).toBe(0);
    });
  });

  it("keeps a row pending without an attempt on retry-later", async () => {
    await runInDurableObject(fresh(), async (_i, state) => {
      const t = mk(state, async () => ({ retry: true, afterMs: 5000 }));
      t.ob.enqueue({ dest: "custom:x", kind: "k", msgId: "r", payload: {} });
      await t.ob.drain();
      const row = t.ob.get("r")!;
      expect(row.attempts).toBe(0);
      expect(row.next_ms).toBe(t.at() + 5000);
    });
  });

  it("backoff doubles from 1 s and caps at 5 min with jitter", () => {
    expect(outboxBackoff(1, () => 0.5)).toBe(1000);
    expect(outboxBackoff(2, () => 0.5)).toBe(2000);
    expect(outboxBackoff(20, () => 0.5)).toBe(300_000);
    expect(outboxBackoff(1, () => 0)).toBe(800);
    expect(outboxBackoff(1, () => 1)).toBe(1200);
  });

  it("parses destinations", () => {
    expect(parseDest(dest.index("default"))).toEqual({ scheme: "do", binding: "INDEX", name: "team:default|index" });
    expect(parseDest("queue:JOBS")).toEqual({ scheme: "queue", queue: "JOBS" });
    expect(parseDest("provider:x")).toEqual({ scheme: "custom", raw: "provider:x" });
    expect(teamOfDoName("team:a|work:w1")).toBe("a");
    expect(teamOfDoName("team:b")).toBe("b");
  });
});

describe("timers and BaseDO", () => {
  it("stores, moves and takes timers", async () => {
    await runInDurableObject(fresh(), async (_i, state) => {
      let now = 1000;
      const t = new Timers(state.storage, () => now);
      t.set("a", "k", 2000, { x: 1 });
      t.setEarliest("a", "k", 3000);
      expect(t.get("a")!.due_ms).toBe(2000);
      t.setEarliest("a", "k", 1500);
      expect(t.nextDue()).toBe(1500);
      expect(t.due()).toHaveLength(0);
      now = 1600;
      const [due] = t.due();
      expect(due!.name).toBe("a");
      t.set("a", "k", 5000);
      expect(t.take(due!)).toBe(false);
      expect(t.clearPrefix("a")).toBe(1);
    });
  });

  it("runs sync timers in the alarm and always re-arms", async () => {
    const stub = fresh();
    const fired: string[] = [];
    await runInDurableObject(stub, async (instance) => {
      const i = instance as unknown as {
        onTimer(k: string, h: (t: { name: string }) => void): void;
        tx<T>(fn: () => T): T;
        setTimer(n: string, k: string, d: number, p?: unknown): void;
      };
      i.onTimer("test.ok", (t) => {
        fired.push(t.name);
      });
      i.onTimer("test.fail", () => {
        throw new Error("timer boom");
      });
      i.tx(() => {
        i.setTimer("t1", "test.ok", Date.now() - 10);
        i.setTimer("t2", "test.fail", Date.now() - 10);
      });
    });
    await runDurableObjectAlarm(stub);
    await eventually(() => expect(fired).toEqual(["t1"]));
    await eventually(() =>
      runInDurableObject(stub, async (_i, state) => {
        const rows = state.storage.sql.exec("SELECT name, attempts FROM timers WHERE name LIKE 't%'").toArray();
        expect(rows).toEqual([{ name: "t2", attempts: 1 }]);
        expect(await state.storage.getAlarm()).not.toBeNull();
      }),
    );
  });

  it("delivers to another DO through receive() with dedupe", async () => {
    const stub = fresh();
    await runInDurableObject(stub, async (instance) => {
      const i = instance as unknown as { tx<T>(fn: () => T): T; enqueue(x: unknown): string };
      i.tx(() =>
        i.enqueue({
          dest: dest.index("libdeliver"),
          kind: "index.deadLetter",
          msgId: "m1",
          payload: { originDO: "team:x", msgId: "orig", kind: "k", lane: "", error: "e", attempts: 12 },
        }),
      );
    });
    await runDurableObjectAlarm(stub);
    await eventually(() =>
      runInDurableObject(stub, async (_i, state) => {
        expect(state.storage.sql.exec("SELECT msg_id, last_error FROM outbox").toArray()).toEqual([]);
      }),
    );
    const index = env.INDEX.getByName("team:libdeliver|index");
    const env1 = { msgId: "m1", kind: "index.deadLetter", lane: "", origin: "team:x", payload: { originDO: "team:x", msgId: "orig", kind: "k", lane: "", error: "e", attempts: 12 } } as const;
    expect(await index.receive(env1 as never)).toEqual({ ok: true });
    await runInDurableObject(index, async (_i, state) => {
      expect(state.storage.sql.exec("SELECT COUNT(*) AS n FROM dead_letters").one().n).toBe(1);
    });
  });
});
