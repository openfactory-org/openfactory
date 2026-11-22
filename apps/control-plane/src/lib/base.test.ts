import { env } from "cloudflare:workers";
import { runDurableObjectAlarm, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";

let n = 0;
const fresh = () => env.TEAM.getByName(`team:committest${++n}`);

interface StepApi {
  tx<T>(fn: () => T): T;
  onCommit(fn: () => void): void;
  onTimer(k: string, h: () => void): void;
  onMessage(k: string, h: () => unknown): void;
  setTimer(n: string, k: string, d: number): void;
  receive(msg: unknown): Promise<unknown>;
}

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

describe("BaseDO post-commit callbacks", () => {
  it("runs callbacks after tx commits and drops them on rollback", async () => {
    await runInDurableObject(fresh(), async (instance, state) => {
      const i = instance as unknown as StepApi;
      const log: string[] = [];
      const rows = () => state.storage.sql.exec("SELECT k FROM meta WHERE k LIKE 'ct.%' ORDER BY k").toArray();

      i.tx(() => {
        state.storage.sql.exec("INSERT INTO meta (k, v) VALUES ('ct.a', '')");
        i.onCommit(() => log.push(`a:${rows().length}`));
        expect(log).toEqual([]);
      });
      expect(log).toEqual(["a:1"]);

      expect(() =>
        i.tx(() => {
          state.storage.sql.exec("INSERT INTO meta (k, v) VALUES ('ct.b', '')");
          i.onCommit(() => log.push("b"));
          throw new Error("roll back");
        }),
      ).toThrow("roll back");
      expect(log).toEqual(["a:1"]);
      expect(rows()).toEqual([{ k: "ct.a" }]);

      // A nested step is a savepoint: its rollback drops only its own callbacks.
      i.tx(() => {
        i.onCommit(() => log.push("outer"));
        try {
          i.tx(() => {
            i.onCommit(() => log.push("inner"));
            throw new Error("inner");
          });
        } catch {
          /* expected */
        }
        i.tx(() => i.onCommit(() => log.push("inner2")));
        expect(log).toEqual(["a:1"]);
      });
      expect(log).toEqual(["a:1", "outer", "inner2"]);
    });
  });

  it("drops callbacks of a message that asks for a retry", async () => {
    await runInDurableObject(fresh(), async (instance) => {
      const i = instance as unknown as StepApi;
      const log: string[] = [];
      let retry = true;
      i.onMessage("test.commit", () => {
        i.onCommit(() => log.push(retry ? "retry" : "ok"));
        return retry ? { retry: true } : undefined;
      });
      const msg = { msgId: "m1", kind: "test.commit", lane: "", origin: "team:x", payload: {} };
      expect(await i.receive(msg)).toEqual({ retry: true });
      expect(log).toEqual([]);
      retry = false;
      expect(await i.receive(msg)).toEqual({ ok: true });
      expect(log).toEqual(["ok"]);
    });
  });

  it("drops callbacks of a sync timer that throws", async () => {
    const stub = fresh();
    const log: string[] = [];
    await runInDurableObject(stub, async (instance) => {
      const i = instance as unknown as StepApi;
      i.onTimer("test.ok", () => i.onCommit(() => log.push("ok")));
      i.onTimer("test.fail", () => {
        i.onCommit(() => log.push("fail"));
        throw new Error("timer boom");
      });
      i.tx(() => {
        i.setTimer("c1", "test.ok", Date.now() - 10);
        i.setTimer("c2", "test.fail", Date.now() - 10);
      });
    });
    await runDurableObjectAlarm(stub);
    await eventually(() => expect(log).toEqual(["ok"]));
  });
});
