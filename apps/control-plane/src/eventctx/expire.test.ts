// Port of internal/eventctx/expire_test.go. The Go keys of the store become
// bundles in R2 (plan rule 9): release removes the bundles of a run, expire
// removes the bundles older than the retention time. The WorkDO rows that
// name the bundles are removed at the run end (src/do/work/eventfiles.test.ts).
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import type { Event } from "@openfactory/core";
import { build, expire, loadBundle, R2_PREFIX, release, RETENTION_MS, storeBundle } from "./index";

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
  it("release removes the bundles of a run only; a second call is no error", async () => {
    const mine = await stored("msg_first", { n: crypto.randomUUID() });
    const later = await stored("msg_later", { n: crypto.randomUUID() });
    const other = await stored("msg_other", { n: crypto.randomUUID() });
    expect(mine.startsWith(R2_PREFIX)).toBe(true);
    // A reference outside the prefix is never removed.
    await env.DATA.put("attachment/att_1", "{}");

    expect(await release(env.DATA, [mine, later, mine, "attachment/att_1"])).toBe(2);
    expect(await loadBundle(env.DATA, mine)).toBeNull();
    expect(await loadBundle(env.DATA, later)).toBeNull();
    expect((await loadBundle(env.DATA, other))?.message_id).toBe("msg_other");
    expect(await env.DATA.head("attachment/att_1")).not.toBeNull();

    await expect(release(env.DATA, [mine])).resolves.toBe(1);
    await expect(release(env.DATA, [])).resolves.toBe(0);
  });

  it("the same bundle is stored once", async () => {
    const p = { n: crypto.randomUUID() };
    expect(await stored("msg_x", p)).toBe(await stored("msg_x", p));
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
