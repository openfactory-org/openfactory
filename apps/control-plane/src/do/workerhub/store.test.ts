// HubStore (the lease cache of WorkerHubDO) and the handshake helpers of
// protocol.ts. The store tests count the SQL statements and the rows that
// come back to JavaScript, to show the cost of slotsUsed() and attach().
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName } from "@openfactory/core";
import type { SandboxTask, TaskStatus } from "@openfactory/sandbox";
import type { WireFrame, WireHello } from "@openfactory/sandbox/microvm";
import { describe, expect, it } from "vitest";
import type { WorkerRecord } from "../team/workers";
import { frameRefusal, handshakeTimeout, helloRefusal, openAttachment, versionLess } from "./protocol";
import { HubStore, statusOf } from "./store";

const RUN = "run_wstorestorestorestorestore";

let seq = 0;
async function hubSql<T>(fn: (sql: SqlStorage) => T): Promise<T> {
  const stub = env.WORKER_HUB.getByName(doName.workerHub("default", `wkr_store${++seq}`));
  await stub.live(); // runs the migrations
  return runInDurableObject(stub, (_i, state) => fn(state.storage.sql));
}

/** SqlStorage that counts statements and the rows read back by JavaScript. */
function counting(sql: SqlStorage): { sql: SqlStorage; n: { execs: number; rows: number } } {
  const n = { execs: 0, rows: 0 };
  const wrapped = {
    exec(query: string, ...args: unknown[]) {
      n.execs++;
      const cur = sql.exec(query, ...args);
      return new Proxy(cur, {
        get(t, k) {
          if (k === "next")
            return () => {
              const r = t.next();
              if (!r.done) n.rows++;
              return r;
            };
          if (k === "toArray")
            return () => {
              const a = t.toArray();
              n.rows += a.length;
              return a;
            };
          const v = Reflect.get(t, k) as unknown;
          return typeof v === "function" ? (v as (...a: unknown[]) => unknown).bind(t) : v;
        },
      });
    },
  } as unknown as SqlStorage;
  return { sql: wrapped, n };
}

function task(i: number): SandboxTask {
  return { taskId: `${RUN}.${i}`, runId: RUN, session: i } as SandboxTask;
}

const status = (i: number, state: TaskStatus["state"]): TaskStatus => ({ taskId: `${RUN}.${i}`, state }) as TaskStatus;

describe("HubStore", () => {
  it("slotsUsed counts in SQL: one statement and one row for any number of tasks", async () => {
    const r = await hubSql((raw) => {
      const st = new HubStore(raw);
      for (let i = 1; i <= 40; i++) {
        st.lease(task(i), status(i, i <= 10 ? "exited" : "running"), i);
        if (i > 35) st.markDestroyed(task(i).taskId, i);
      }
      // The former computation: every row read and parsed in JavaScript.
      const before = counting(raw);
      const old = new HubStore(before.sql)
        .tasks()
        .filter((t) => t.destroyed === 0 && statusOf(t).state !== "exited").length;
      const after = counting(raw);
      const used = new HubStore(after.sql).slotsUsed();
      return { old, used, oldRows: before.n.rows, rows: after.n.rows, execs: after.n.execs };
    });
    expect(r.used).toBe(25);
    expect(r.used).toBe(r.old);
    expect(r.oldRows).toBe(40);
    expect(r.rows).toBe(1);
    expect(r.execs).toBe(1);
  });

  it("attach replaces the cache and reads it once, however many unleased tasks the hello holds", async () => {
    const r = await hubSql((raw) => {
      const st = new HubStore(raw);
      for (let i = 1; i <= 5; i++) st.lease(task(i), status(i, "running"), i);
      st.markDestroyed(task(5).taskId, 5);
      const held = new Map<string, TaskStatus>();
      held.set(task(1).taskId, status(1, "running"));
      held.set(task(2).taskId, status(2, "exited"));
      held.set(task(5).taskId, status(5, "exited"));
      for (let i = 100; i < 120; i++) held.set(task(i).taskId, status(i, "running"));
      const c = counting(raw);
      const res = new HubStore(c.sql).attach(held, 1000);
      return {
        res,
        execs: c.n.execs,
        rows: st.tasks().map((t) => [t.task_id, statusOf(t).state, t.destroyed]),
      };
    });
    // 1 read + 2 deletes (tasks 3 and 4) + 2 updates (tasks 1 and 2); none per unleased task.
    expect(r.execs).toBe(5);
    expect(r.res.exited.map((s) => s.taskId)).toEqual([task(2).taskId]);
    expect(r.res.ignored).toHaveLength(20);
    expect(r.rows).toEqual([
      [task(1).taskId, "running", 0],
      [task(2).taskId, "exited", 0],
      [task(5).taskId, "running", 1], // a destroy still owed keeps its row and status
    ]);
  });

  it("remove, markDestroyed and destroyedIds", async () => {
    const r = await hubSql((raw) => {
      const st = new HubStore(raw);
      st.lease(task(1), status(1, "running"), 1);
      st.lease(task(2), status(2, "running"), 2);
      return {
        missingMark: st.markDestroyed(task(9).taskId, 3),
        mark: st.markDestroyed(task(2).taskId, 3),
        destroyed: st.destroyedIds(),
        live: st.liveStatuses().map((s) => s.taskId),
        removed: st.remove(task(1).taskId),
        removedAgain: st.remove(task(1).taskId),
      };
    });
    expect(r).toEqual({
      missingMark: false,
      mark: true,
      destroyed: [task(2).taskId],
      live: [task(1).taskId],
      removed: true,
      removedAgain: false,
    });
  });
});

describe("handshake", () => {
  const rec = { id: "wkr_a", publicKey: "k" } as WorkerRecord;
  const hello = (h: Partial<WireHello>): WireHello => ({ worker_id: "wkr_a", protocol_minor: 1, ...h }) as WireHello;

  it.each([
    ["accepts", rec, "key", hello({}), "", null],
    ["refuses another worker ID", rec, "key", hello({ worker_id: "wkr_b" }), "", "worker_id does not match"],
    ["refuses without a record", undefined, "key", hello({}), "", "worker_id does not match"],
    ["refuses protocol minor 0 for a key", rec, "key", hello({ protocol_minor: 0 }), "", "runtime_too_old"],
    ["accepts protocol minor 0 for an API key", rec, "apikey", hello({ protocol_minor: 0 }), "", null],
    ["refuses a version below the minimum", rec, "key", hello({ version: "1.2.0" }), "v1.3", "runtime_too_old"],
    ["accepts the minimum version", rec, "key", hello({ version: "1.3.0" }), "v1.3", null],
  ] as const)("hello: %s", (_name, r, mode, h, min, reason) => {
    const got = helloRefusal(r, openAttachment("wconn_1", mode, 0), h, min);
    expect(got?.reason ?? null).toBe(reason);
  });

  it("frames outside their phase close the socket", () => {
    const f = (type: string) => ({ type }) as WireFrame;
    expect(frameRefusal("challenge", f("hello"))).toEqual({ code: 1008, reason: "auth_failed" });
    expect(frameRefusal("challenge", f("auth.response"))).toBeNull();
    expect(frameRefusal("hello", f("event"))).toEqual({ code: 1002, reason: "expected hello" });
    expect(frameRefusal("hello", f("hello"))).toBeNull();
    expect(frameRefusal("ready", f("event"))).toBeNull();
    expect(handshakeTimeout("challenge").reason).toBe("auth_failed");
    expect(handshakeTimeout("hello").reason).toBe("expected hello");
  });

  it("a key connection starts with a nonce, an API key connection with hello", () => {
    const k = openAttachment("wconn_1", "key", 5);
    expect(k.phase).toBe("challenge");
    expect(k.nonce).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(openAttachment("wconn_2", "apikey", 5)).toEqual({
      connId: "wconn_2",
      mode: "apikey",
      phase: "hello",
      openedMs: 5,
      lastSeenMs: 5,
    });
  });

  it.each([
    ["1.2.0", "1.3.0", true],
    ["v1.10", "v1.9", false],
    ["1.3", "1.3.0", false],
    ["dev", "1.0.0", false],
  ] as const)("versionLess(%s, %s) = %s", (a, b, want) => {
    expect(versionLess(a, b)).toBe(want);
  });
});
