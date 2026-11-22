// DispatchDO admission (port of internal/dispatch/limits_test.go, asserted
// exact): concurrent claims never exceed maxConcurrentRuns; grants are FIFO.
import { env } from "cloudflare:workers";
import { doName, newRoutedId, newToken } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { Ack, OutboxEnvelope, OutboxKind, OutboxPayloads } from "../../contracts/do-messages";

interface DispatchApi {
  receive(m: OutboxEnvelope): Promise<Ack>;
  state(): Promise<{ waiting: { run_id: string; session: number; hold: string }[]; leases: { run_id: string; session: number; factory_id: string }[] }>;
  spendGate(f: string): Promise<{ allowed: boolean }>;
}

function dispatch(team: string): DispatchApi {
  return env.DISPATCH.getByName(doName.dispatch(team)) as unknown as DispatchApi;
}

let n = 0;
function msg<K extends OutboxKind>(kind: K, payload: OutboxPayloads[K]): OutboxEnvelope<K> {
  return { msgId: `t:${kind}:${++n}:${Math.random()}`, kind, lane: "dispatch", origin: "test", payload };
}

const T0 = Date.now();
function request(runId: string, factoryId: string, offset: number, providerId = "local") {
  const queuedMs = T0 + offset;
  return msg("dispatch.request", {
    runId,
    session: 1,
    factoryId,
    target: { kind: "provider", providerId },
    queuedMs,
    runCreatedMs: queuedMs,
    restricted: false,
    workDO: doName.work("t", newToken("w")),
  });
}

describe("DispatchDO admission", () => {
  it("two concurrent claims never exceed maxConcurrentRuns; FIFO grant order", async () => {
    const team = "lim" + newToken("w");
    const d = dispatch(team);
    await d.receive(msg("dispatch.limits", { factoryId: "fac_a", versionId: "v1", maxConcurrentRuns: 1, spendLimit: null }));
    const tok = newToken("w");
    const runs = [1, 2, 3].map(() => newRoutedId("run", tok));
    // Received one by one in order, then concurrently: the DO decides each in one transaction.
    await d.receive(request(runs[0]!, "fac_a", 1000));
    await Promise.all([d.receive(request(runs[1]!, "fac_a", 2000)), d.receive(request(runs[2]!, "fac_a", 3000))]);
    let st = await d.state();
    expect(st.leases).toHaveLength(1);
    expect(st.leases[0]!.run_id).toBe(runs[0]);
    expect(st.waiting.map((w) => w.run_id)).toEqual(runs.slice(1).sort((a, b) => runs.indexOf(a) - runs.indexOf(b)));

    // A duplicate request changes nothing.
    await d.receive(request(runs[1]!, "fac_a", 2000));
    expect((await d.state()).waiting).toHaveLength(2);

    await d.receive(msg("dispatch.release", { runId: runs[0]!, session: 1, computeSeconds: 1, endReason: "idle" }));
    st = await d.state();
    expect(st.leases.map((l) => l.run_id)).toEqual([runs[1]]);
    expect(st.waiting.map((w) => w.run_id)).toEqual([runs[2]]);

    await d.receive(msg("dispatch.release", { runId: runs[1]!, session: 1, computeSeconds: 1, endReason: "idle" }));
    st = await d.state();
    expect(st.leases.map((l) => l.run_id)).toEqual([runs[2]]);
    expect(st.waiting).toHaveLength(0);
  });

  it("many concurrent requests across two factories respect each limit", async () => {
    const team = "lim2" + newToken("w");
    const d = dispatch(team);
    await d.receive(msg("dispatch.limits", { factoryId: "fa", versionId: "v", maxConcurrentRuns: 2, spendLimit: null }));
    await d.receive(msg("dispatch.limits", { factoryId: "fb", versionId: "v", maxConcurrentRuns: 1, spendLimit: null }));
    const tok = newToken("w");
    await Promise.all(
      Array.from({ length: 10 }, (_, i) => d.receive(request(newRoutedId("run", tok), i % 2 ? "fa" : "fb", 1000 + i))),
    );
    const st = await d.state();
    expect(st.leases.filter((l) => l.factory_id === "fa")).toHaveLength(2);
    expect(st.leases.filter((l) => l.factory_id === "fb")).toHaveLength(1);
    expect(st.waiting).toHaveLength(7);
  });

  it("boat.dev start-rate bucket: 12 a minute", async () => {
    const team = "bkt" + newToken("w");
    const d = dispatch(team);
    const tok = newToken("w");
    for (let i = 0; i < 14; i++) await d.receive(request(newRoutedId("run", tok), "fx", 1000 + i, "boatdev"));
    const st = await d.state();
    expect(st.leases).toHaveLength(12);
    expect(st.waiting).toHaveLength(2);
  });

  it("spend over the factory limit holds new requests (C5: bucketed by run creation day)", async () => {
    const team = "spd" + newToken("w");
    const d = dispatch(team);
    await d.receive(msg("dispatch.limits", { factoryId: "fs", versionId: "v", maxConcurrentRuns: 0, spendLimit: { daily: 1 } }));
    const tok = newToken("w");
    const r1 = newRoutedId("run", tok);
    const now = Date.now();
    await d.receive(msg("dispatch.usage", { runId: r1, session: 1, seq: 5, factoryId: "fs", runCreatedMs: now, costUsd: 0.6 }));
    // A lower seq is ignored; a cumulative value replaces, not adds.
    await d.receive(msg("dispatch.usage", { runId: r1, session: 1, seq: 4, factoryId: "fs", runCreatedMs: now, costUsd: 9 }));
    expect((await d.spendGate("fs")).allowed).toBe(true);
    await d.receive(msg("dispatch.usage", { runId: r1, session: 1, seq: 6, factoryId: "fs", runCreatedMs: now, costUsd: 1.2 }));
    expect((await d.spendGate("fs")).allowed).toBe(false);
    // Cost of a run created yesterday does not count today.
    const r2 = newRoutedId("run", tok);
    await d.receive(msg("dispatch.usage", { runId: r2, session: 1, seq: 1, factoryId: "fs2", runCreatedMs: now - 86_400_000 * 2, costUsd: 5 }));
    await d.receive(msg("dispatch.limits", { factoryId: "fs2", versionId: "v", maxConcurrentRuns: 0, spendLimit: { daily: 1 } }));
    expect((await d.spendGate("fs2")).allowed).toBe(true);

    const r3 = newRoutedId("run", tok);
    await d.receive(request(r3, "fs", 0));
    const st = await d.state();
    expect(st.leases.some((l) => l.run_id === r3)).toBe(false);
    expect(st.waiting.find((w) => w.run_id === r3)?.hold).toBe("spend");
  });
});
