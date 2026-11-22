import { env } from "cloudflare:workers";
import { runDurableObjectAlarm, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { Timers, type TimerRow } from "./timers";

let n = 0;
const fresh = () => env.TEAM.getByName(`team:timertest${++n}`);

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

describe("Timers row version", () => {
  const cases: { name: string; reset: (t: Timers, fired: TimerRow) => void }[] = [
    { name: "set again to the same due_ms", reset: (t, f) => t.set(f.name, f.kind, f.due_ms) },
    {
      name: "cleared and set again to the same due_ms",
      reset: (t, f) => {
        t.clear(f.name);
        t.set(f.name, f.kind, f.due_ms);
      },
    },
  ];
  for (const c of cases) {
    it(`take and fail keep a timer ${c.name}`, async () => {
      await runInDurableObject(fresh(), async (_i, state) => {
        let now = 1000;
        const t = new Timers(state.storage, () => now);
        t.set("a", "k", 1500);
        now = 2000;
        const [fired] = t.due();
        expect(fired!.name).toBe("a");
        c.reset(t, fired!);
        expect(t.take(fired!)).toBe(false);
        t.fail(fired!);
        const row = t.get("a")!;
        expect(row.due_ms).toBe(1500);
        expect(row.attempts).toBe(0);
        expect(row.seq).toBeGreaterThan(fired!.seq);
        // The unchanged row is taken.
        expect(t.take(row)).toBe(true);
        expect(t.get("a")).toBeUndefined();
      });
    });
  }

  it("keeps a timer that an async handler sets again to the same due_ms", async () => {
    const stub = fresh();
    const calls: number[] = [];
    await runInDurableObject(stub, async (instance) => {
      const i = instance as unknown as {
        onTimerAsync(k: string, h: (t: TimerRow) => Promise<void>): void;
        tx<T>(fn: () => T): T;
        setTimer(n: string, k: string, d: number): void;
      };
      i.onTimerAsync("test.again", async (t) => {
        calls.push(t.due_ms);
        await null;
        if (calls.length === 1) i.tx(() => i.setTimer(t.name, t.kind, t.due_ms));
      });
      i.tx(() => i.setTimer("again", "test.again", Date.now() - 10));
    });
    await runDurableObjectAlarm(stub);
    await eventually(() => expect(calls).toHaveLength(2));
    expect(calls[0]).toBe(calls[1]);
    await eventually(() =>
      runInDurableObject(stub, async (_i, state) => {
        expect(state.storage.sql.exec("SELECT name FROM timers WHERE name = 'again'").toArray()).toEqual([]);
      }),
    );
  });
});
