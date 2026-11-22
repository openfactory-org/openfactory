// DispatchDO admission (port of internal/dispatch/limits_test.go, asserted
// exact): concurrent claims never exceed maxConcurrentRuns; grants are FIFO.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, newRoutedId, newToken } from "@openfactory/core";
import type { SandboxProvider, TaskStatus } from "@openfactory/sandbox";
import { describe, expect, it, vi } from "vitest";
import type { Ack, OutboxEnvelope, OutboxKind, OutboxPayloads } from "../../contracts/do-messages";
import { registerProvider, unregisterProvider } from "./providers";

interface DispatchApi {
  receive(m: OutboxEnvelope): Promise<Ack>;
  state(): Promise<{
    waiting: { run_id: string; session: number; hold: string }[];
    leases: { run_id: string; session: number; factory_id: string }[];
  }>;
  spendGate(f: string): Promise<{ allowed: boolean }>;
  pump(): Promise<void>;
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
    await d.receive(
      msg("dispatch.limits", { factoryId: "fac_a", versionId: "v1", maxConcurrentRuns: 1, spendLimit: null }),
    );
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
    await d.receive(
      msg("dispatch.limits", { factoryId: "fa", versionId: "v", maxConcurrentRuns: 2, spendLimit: null }),
    );
    await d.receive(
      msg("dispatch.limits", { factoryId: "fb", versionId: "v", maxConcurrentRuns: 1, spendLimit: null }),
    );
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

  it("spend over the factory limit holds new requests (bucketed by run creation day)", async () => {
    const team = "spd" + newToken("w");
    const d = dispatch(team);
    await d.receive(
      msg("dispatch.limits", { factoryId: "fs", versionId: "v", maxConcurrentRuns: 0, spendLimit: { daily: 1 } }),
    );
    const tok = newToken("w");
    const r1 = newRoutedId("run", tok);
    const now = Date.now();
    await d.receive(
      msg("dispatch.usage", { runId: r1, session: 1, seq: 5, factoryId: "fs", runCreatedMs: now, costUsd: 0.6 }),
    );
    // A lower seq is ignored; a cumulative value replaces, not adds.
    await d.receive(
      msg("dispatch.usage", { runId: r1, session: 1, seq: 4, factoryId: "fs", runCreatedMs: now, costUsd: 9 }),
    );
    expect((await d.spendGate("fs")).allowed).toBe(true);
    await d.receive(
      msg("dispatch.usage", { runId: r1, session: 1, seq: 6, factoryId: "fs", runCreatedMs: now, costUsd: 1.2 }),
    );
    expect((await d.spendGate("fs")).allowed).toBe(false);
    // Cost of a run created yesterday does not count today.
    const r2 = newRoutedId("run", tok);
    await d.receive(
      msg("dispatch.usage", {
        runId: r2,
        session: 1,
        seq: 1,
        factoryId: "fs2",
        runCreatedMs: now - 86_400_000 * 2,
        costUsd: 5,
      }),
    );
    await d.receive(
      msg("dispatch.limits", { factoryId: "fs2", versionId: "v", maxConcurrentRuns: 0, spendLimit: { daily: 1 } }),
    );
    expect((await d.spendGate("fs2")).allowed).toBe(true);

    const r3 = newRoutedId("run", tok);
    await d.receive(request(r3, "fs", 0));
    const st = await d.state();
    expect(st.leases.some((l) => l.run_id === r3)).toBe(false);
    expect(st.waiting.find((w) => w.run_id === r3)?.hold).toBe("spend");
  });
});

describe("DispatchDO usage", () => {
  it("re-runs the admission pass only when the factory crosses its spend limit", async () => {
    const team = "use" + newToken("w");
    const stub = env.DISPATCH.getByName(doName.dispatch(team));
    const d = dispatch(team);
    await d.receive(
      msg("dispatch.limits", { factoryId: "fu", versionId: "v", maxConcurrentRuns: 1, spendLimit: { daily: 1 } }),
    );
    const tok = newToken("w");
    const leased = newRoutedId("run", tok);
    const queued = newRoutedId("run", tok);
    await d.receive(request(leased, "fu", 1000));
    await d.receive(request(queued, "fu", 2000));
    const now = Date.now();
    await runInDurableObject(stub, async (instance) => {
      const i = instance as unknown as DispatchApi & { admission(): void };
      const spy = vi.spyOn(i, "admission");
      const usage = (seq: number, costUsd: number, factoryId = "fu") =>
        i.receive(msg("dispatch.usage", { runId: leased, session: 1, seq, factoryId, runCreatedMs: now, costUsd }));
      await usage(1, 0.3);
      await usage(2, 0.9);
      expect(spy).toHaveBeenCalledTimes(0);
      await usage(3, 1.5);
      expect(spy).toHaveBeenCalledTimes(1);
      await usage(4, 2);
      expect(spy).toHaveBeenCalledTimes(1);
      spy.mockRestore();
    });
    // The pass that ran on the crossing put the queued row on hold.
    const st = await d.state();
    expect(st.waiting.find((w) => w.run_id === queued)?.hold).toBe("spend");
    expect(st.leases.map((l) => l.run_id)).toEqual([leased]);
  });
});

describe("DispatchDO spend hold rollover", () => {
  it("a spend hold arms a timer at the UTC day rollover, and the timer releases the row", async () => {
    const team = "roll" + newToken("w");
    const d = dispatch(team);
    await d.receive(
      msg("dispatch.limits", { factoryId: "fr", versionId: "v", maxConcurrentRuns: 0, spendLimit: { daily: 1 } }),
    );
    const tok = newToken("w");
    const now = Date.now();
    await d.receive(
      msg("dispatch.usage", {
        runId: newRoutedId("run", tok),
        session: 1,
        seq: 1,
        factoryId: "fr",
        runCreatedMs: now,
        costUsd: 2,
      }),
    );
    const held = newRoutedId("run", tok);
    await d.receive(request(held, "fr", 0));
    expect((await d.state()).waiting.find((w) => w.run_id === held)?.hold).toBe("spend");

    const stub = env.DISPATCH.getByName(doName.dispatch(team));
    await runInDurableObject(stub, (_i, state) => {
      const sql = state.storage.sql;
      const t = sql.exec<{ due_ms: number }>("SELECT due_ms FROM timers WHERE name = 'spend'").toArray();
      const n = new Date();
      expect(t.map((r) => r.due_ms)).toEqual([Date.UTC(n.getUTCFullYear(), n.getUTCMonth(), n.getUTCDate() + 1)]);
      // Simulate the rollover: the spend now belongs to a past day, and the timer is due.
      sql.exec("UPDATE spend_totals SET start = '2000-01-01' WHERE period = 'daily'");
      sql.exec("UPDATE timers SET due_ms = 1 WHERE name = 'spend'");
    });
    await d.pump();
    const st = await d.state();
    expect(st.waiting.some((w) => w.run_id === held)).toBe(false);
    expect(st.leases.some((l) => l.run_id === held)).toBe(true);
  });
});

describe("DispatchDO orphan sweep", () => {
  it("re-reads leases after list() and spares tasks without startedAt until they are old", async () => {
    const team = "swp" + newToken("w");
    const stub = env.DISPATCH.getByName(doName.dispatch(team));
    const tok = newToken("w");
    const old = new Date(Date.now() - 10 * 60_000).toISOString();
    const granted = `${newRoutedId("run", tok)}.1`;
    const noStart = `${newRoutedId("run", tok)}.1`;
    const orphan = `${newRoutedId("run", tok)}.1`;
    const destroyed: string[] = [];
    const id = "sweeptest-" + newToken("w");
    let onList: () => void = () => undefined;
    const provider = {
      id,
      async list(): Promise<TaskStatus[]> {
        await new Promise((r) => setTimeout(r, 1));
        // A grant lands while the provider answers.
        onList();
        return [
          { taskId: granted, state: "running", startedAt: old },
          { taskId: noStart, state: "accepted" },
          { taskId: orphan, state: "running", startedAt: old },
          { taskId: "not-a-task-id", state: "running", startedAt: old },
        ];
      },
      async destroy(taskId: string) {
        destroyed.push(taskId);
      },
    } as unknown as SandboxProvider;
    registerProvider(id, () => provider);
    try {
      await runInDurableObject(stub, async (instance, state) => {
        const sql = state.storage.sql;
        onList = () =>
          sql.exec(
            `INSERT INTO leases (run_id, session, task_id, factory_id, target_key, provider, work_do, granted_ms)
             VALUES (?, 1, ?, 'f', ?, ?, 'w', ?)`,
            granted.slice(0, -2),
            granted,
            `provider:${id}`,
            id,
            Date.now(),
          );
        const sweep = () => (instance as unknown as { sweep(): Promise<void> }).sweep();
        await sweep();
        expect(destroyed).toEqual([orphan]);
        // Still young by first sight: a second sweep spares it too.
        onList = () => undefined;
        await sweep();
        expect(destroyed.includes(noStart)).toBe(false);
        expect(destroyed.includes(granted)).toBe(false);
      });
    } finally {
      unregisterProvider(id);
    }
  });
});
