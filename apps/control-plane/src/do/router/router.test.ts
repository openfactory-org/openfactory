// RouterDO and IngressDO: atomic reserve, aliases, reopen; dedupe, order,
// retry with backoff and redrive.
import { env } from "cloudflare:workers";
import { doName, newToken } from "@openfactory/core";
import { afterEach, describe, expect, it } from "vitest";
import type { CoreEvent } from "../../contracts/do-messages";
import { setIntakeHandler, type IngressDO } from "../ingress/index";
import type { RouterDO } from "./index";

const router = (t: string) => env.ROUTER.getByName(doName.router(t)) as unknown as RouterDO;
const ingress = (t: string, p: string) => env.INGRESS.getByName(doName.ingress(t, p)) as unknown as IngressDO;

describe("RouterDO", () => {
  it("resolveOrReserve is atomic and gives one work item per source key", async () => {
    const t = "r" + newToken("w");
    const r = router(t);
    const [a, b] = await Promise.all([
      r.resolveOrReserve({ factoryId: "f", sourceKeys: ["github:o/r#1"], actionKey: "ak1", target: "auto:1" }),
      r.resolveOrReserve({ factoryId: "f", sourceKeys: ["github:o/r#1"], actionKey: "ak2", target: "auto:1" }),
    ]);
    expect(a.wiId).toBe(b.wiId);
    expect([a.created, b.created].filter(Boolean)).toHaveLength(1);
    // Without a source key the token is deterministic: a retry reaches the same WorkDO.
    const c1 = await r.resolveOrReserve({ factoryId: "f", sourceKeys: [], actionKey: "ak3", target: "auto:2" });
    const c2 = await r.resolveOrReserve({ factoryId: "f", sourceKeys: [], actionKey: "ak3", target: "auto:2" });
    expect(c1.token).toBe(c2.token);
    expect((await r.resolve("f", "github:o/r#1"))?.wiId).toBe(a.wiId);
  });

  it("addReference refuses a key owned by another active work item; reopen checks other actives", async () => {
    const t = "r" + newToken("w");
    const r = router(t);
    const a = await r.resolveOrReserve({ factoryId: "f", sourceKeys: ["k1"], actionKey: "x", target: "auto:1" });
    const b = await r.resolveOrReserve({ factoryId: "f", sourceKeys: ["k2"], actionKey: "y", target: "auto:1" });
    expect(
      await r.addReference({ factoryId: "f", key: "k1", kind: "pull_request", wiId: b.wiId, token: b.token }),
    ).toEqual({ ok: false, ownerWiId: a.wiId });
    expect(
      await r.addReference({ factoryId: "f", key: "pr:9", kind: "pull_request", wiId: b.wiId, token: b.token }),
    ).toEqual({ ok: true });
    expect((await r.resolve("f", "pr:9"))?.wiId).toBe(b.wiId);
    await r.receive({
      msgId: "s1",
      kind: "router.status",
      lane: "router",
      origin: "t",
      payload: { wiId: a.wiId, factoryId: "f", stage: "complete", completedMs: Date.now(), version: 3, keys: ["k1"] },
    });
    expect((await r.resolve("f", "k1"))?.status).toBe("completed");
    expect(await r.reopen({ factoryId: "f", sourceKey: "k1", wiId: a.wiId })).toBe(true);
    expect((await r.resolve("f", "k1"))?.status).toBe("active");
  });

  it("a route rebuilt from a status message keeps the source key's owner", async () => {
    const t = "r" + newToken("w");
    const r = router(t);
    // A fresh RouterDO, as after a reset: only the rebuild message brings the routes back.
    const a = { wiId: "wi_" + newToken("w") + "x", token: "" };
    await r.receive({
      msgId: "rebuild:a",
      kind: "router.status",
      lane: "router",
      origin: "t",
      payload: {
        wiId: a.wiId,
        factoryId: "f",
        stage: "running",
        completedMs: null,
        version: 2,
        keys: ["github:o/r#5", "pr:5"],
        sourceKey: "github:o/r#5",
      } as never,
    });
    const b = await r.resolveOrReserve({ factoryId: "f", sourceKeys: ["k9"], actionKey: "z", target: "auto:1" });
    // The source key is still owned by a; a reference key is not.
    expect(
      await r.addReference({ factoryId: "f", key: "github:o/r#5", kind: "issue", wiId: b.wiId, token: b.token }),
    ).toEqual({ ok: false, ownerWiId: a.wiId });
    expect(
      await r.addReference({ factoryId: "f", key: "pr:5", kind: "pull_request", wiId: b.wiId, token: b.token }),
    ).toEqual({ ok: true });
    expect((await r.resolve("f", "github:o/r#5"))?.wiId).toBe(a.wiId);
  });
});

const ev = (id: string, text = ""): CoreEvent => ({
  id,
  team_id: "t",
  provider: "webhook",
  type: "received",
  actor: { provider: "webhook", external_id: "x", handle: "x", is_bot: false },
  source: {},
  attributes: {},
  text,
  received_at: new Date().toISOString(),
});

describe("IngressDO", () => {
  afterEach(() => setIntakeHandler(async () => ({ done: true })));

  it("dedupes and delivers in order; a transient failure retries the head", async () => {
    const seen: string[] = [];
    let failOnce = true;
    setIntakeHandler(async (_env, _ctx, e) => {
      if (e.id === "e2" && failOnce) {
        failOnce = false;
        return { retry: true, error: "transient" };
      }
      seen.push(e.id);
      return { done: true };
    });
    const t = "i" + newToken("w");
    const g = ingress(t, "webhook:w1");
    expect(await g.enqueueEvent("webhook:w1", ev("e1"), "e1")).toEqual({ accepted: true, deduplicated: false });
    expect(await g.enqueueEvent("webhook:w1", ev("e1"), "e1")).toEqual({ accepted: false, deduplicated: true });
    await g.enqueueEvent("webhook:w1", ev("e2"), "e2");
    await g.enqueueEvent("webhook:w1", ev("e3"), "e3");
    await g.pump();
    // e3 waits behind the failed e2.
    expect(seen).toEqual(["e1"]);
    const list = (await g.deliveries(null, "listEvents", {})) as {
      events: { id: string; state: string; attempts: number }[];
    };
    expect(list.events.find((x) => x.id === "e2")).toMatchObject({ state: "pending", attempts: 1 });
  });
});
