// IngressDO delivery loop: one intake call per event when alarms overlap, and
// a full batch re-arms a timer that the alarm does not delete.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, newToken } from "@openfactory/core";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { CoreEvent } from "../../contracts/do-messages";
import { setIntakeHandler, type IngressDO } from "./index";

const ingress = (t: string, p: string) => env.INGRESS.getByName(doName.ingress(t, p));

const ev = (id: string): CoreEvent => ({
  id,
  team_id: "t",
  provider: "webhook",
  type: "webhook.received",
  actor: { provider: "webhook", external_id: "x", handle: "x", is_bot: false },
  source: {},
  attributes: {},
  text: "",
  received_at: new Date().toISOString(),
});

async function eventually(check: () => Promise<void> | void, ms = 3000): Promise<void> {
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

describe("IngressDO delivery loop", () => {
  afterEach(() => setIntakeHandler(async () => ({ done: true })));

  it("calls intake once per event when pump() overlaps the alarm", async () => {
    const calls: string[] = [];
    setIntakeHandler(async (_env, _ctx, e) => {
      calls.push(e.id);
      await new Promise((r) => setTimeout(r, 50));
      return { done: true };
    });
    const g = ingress("i" + newToken("w"), "webhook:w1") as unknown as IngressDO;
    // enqueue() starts an alarm pass with waitUntil; the platform alarm and pump() overlap it.
    await g.enqueue("webhook:w1", ev("e1"), "e1");
    await Promise.all([g.pump(), g.pump()]);
    await eventually(async () => {
      const list = (await g.deliveries(null, "listEvents", {})) as { events: { state: string }[] };
      expect(list.events.map((x) => x.state)).toEqual(["done"]);
    });
    expect(calls).toEqual(["e1"]);
  });

  it("delivers past the 25-event batch limit", async () => {
    const stub = ingress("i" + newToken("w"), "webhook:w2");
    await runInDurableObject(stub, async (instance, state) => {
      const i = instance as unknown as {
        tx<T>(fn: () => T): T;
        insert(e: CoreEvent, key: string): unknown;
        alarm(): Promise<void>;
      };
      i.tx(() => {
        for (let k = 0; k < 26; k++) i.insert(ev(`b${k}`), `b${k}`);
      });
      // Keep the platform alarm out of this pass.
      await state.storage.deleteAlarm();
      // A fast batch ends at the due time of the timer that fired.
      const t0 = Date.now();
      state.storage.sql.exec("UPDATE timers SET due_ms = ? WHERE name = 'deliver'", t0);
      const clock = vi.spyOn(Date, "now").mockReturnValue(t0);
      try {
        await i.alarm();
      } finally {
        clock.mockRestore();
      }
      const sql = state.storage.sql;
      expect(sql.exec("SELECT COUNT(*) AS n FROM events WHERE state = 'pending'").one().n).toBe(1);
      expect(sql.exec("SELECT name FROM timers WHERE name = 'deliver'").toArray()).toHaveLength(1);
    });
    await eventually(async () => {
      const list = (await (stub as unknown as IngressDO).deliveries(null, "listEvents", {})) as {
        events: { state: string }[];
      };
      expect(list.events).toHaveLength(26);
      expect(list.events.every((x) => x.state === "done")).toBe(true);
    });
  });
});
