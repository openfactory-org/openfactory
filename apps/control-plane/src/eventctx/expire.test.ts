// Port of internal/eventctx/expire_test.go. The Go keys of the store become
// bundles in R2 (a DO row holds at most 8 MB). Runs share a bundle key, so a run end does not
// remove bundles: expire removes the bundles older than the retention time.
// The WorkDO rows that name the bundles are removed at the run end
// (src/do/work/eventfiles.test.ts).
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import type { Event } from "@openfactory/core";
import { build, expire, loadBundle, R2_PREFIX, RETENTION_MS, storeBundle } from "./index";

const ev = (payload: unknown): Event =>
  ({
    id: "e",
    team_id: "t",
    provider: "webhook",
    type: "received",
    actor: { provider: "webhook" },
    source: { provider: "webhook", kind: "delivery" },
    received_at: "2026-01-01T00:00:00Z",
    payload,
  }) as unknown as Event;

async function stored(messageId: string, payload: unknown): Promise<string> {
  const b = await build(messageId, ev(payload), false);
  return storeBundle(env.DATA, b!);
}

describe("event context release and expiry", () => {
  it("the same bundle is stored once", async () => {
    const p = { n: crypto.randomUUID() };
    const ref = await stored("msg_x", p);
    expect(ref.startsWith(R2_PREFIX)).toBe(true);
    expect(await stored("msg_x", p)).toBe(ref);
    expect((await loadBundle(env.DATA, ref))?.message_id).toBe("msg_x");
  });

  it("expire removes old bundles only", async () => {
    const a = await stored("msg_1", { n: crypto.randomUUID() });
    await env.DATA.put("attachment/att_2", "{}");
    expect(await expire(env.DATA, Date.now())).toBe(0);
    expect(await env.DATA.head(a)).not.toBeNull();
    expect(await expire(env.DATA, Date.now() + RETENTION_MS + 60_000)).toBeGreaterThanOrEqual(1);
    expect(await env.DATA.head(a)).toBeNull();
    expect(await env.DATA.head("attachment/att_2")).not.toBeNull();
  });

  it("loadBundle refuses a reference outside the prefix", async () => {
    await expect(loadBundle(env.DATA, "attachment/att_1")).rejects.toThrow(/bad bundle reference/);
  });
});
