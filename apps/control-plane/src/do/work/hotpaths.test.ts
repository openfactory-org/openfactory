// The hot paths of WorkDO: query plans that use their indexes, one query for
// the snapshot parts of all sessions, one session write per event batch, and a
// retention sweep that reads finished runs one page at a time.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { afterEach, describe, expect, it } from "vitest";
import type { RunSession } from "../../engine/types";
import { buildRuns, settle, withFx, workStub } from "../../orchestration/fixture.test-util";
import { sessionViews } from "./api-handlers";
import { retentionStep, type WorkHost } from "./effects";
import { DIAGNOSTICS_SQL, Repo, SENT_MESSAGES_SQL } from "./repo";

afterEach(() => settle());

const meta = { requestId: "t" };

function plan(sql: SqlStorage, q: string, ...args: unknown[]): string {
  return sql
    .exec<{ detail: string }>("EXPLAIN QUERY PLAN " + q, ...args)
    .toArray()
    .map((r) => r.detail)
    .join("\n");
}

async function oneRun(state: "running" | "succeeded" = "running") {
  const f = await buildRuns({}, { r: { state } });
  return { token: f.token, runId: f.ids.r! };
}

function activeSession(runId: string): RunSession {
  const now = new Date().toISOString();
  return {
    run_id: runId,
    session: 1,
    task_id: `${runId}.1`,
    state: "active",
    bootstrap_expires_at: now,
    assigned_at: now,
    usage: { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 },
    version: 0,
    event_ack: 0,
    command_seq: 0,
    command_ack: 0,
    last_seen_at: now,
  };
}

describe("WorkDO query plans", () => {
  it("getRunDiagnostics reads the partial index and keeps session and seq order", async () => {
    const { token, runId } = await oneRun();
    const detail = await withFx(token, (_fx, sql) => {
      const ins = (session: number, seq: number, type: string, data: unknown) =>
        sql.exec(
          "INSERT INTO runner_events (run_id, session, seq, type, turn_id, at_ms, data) VALUES (?, ?, ?, ?, '', ?, ?)",
          runId,
          session,
          seq,
          type,
          1_700_000_000_000 + seq,
          JSON.stringify(data),
        );
      ins(2, 1, "diagnostic", { n: 3 });
      ins(1, 1, "message", {});
      ins(1, 2, "diagnostic", { n: 1 });
      ins(1, 3, "diagnostic", { n: 2 });
      return plan(sql, DIAGNOSTICS_SQL, runId);
    });
    expect(detail).toContain("runner_events_diag");
    const out = (await workStub(token).api(meta, "getRunDiagnostics", { runId }, null)) as {
      diagnostics: { session: number; seq: number; data: { n: number } }[];
    };
    expect(out.diagnostics.map((e) => [e.session, e.seq, e.data.n])).toEqual([
      [1, 2, 1],
      [1, 3, 2],
      [2, 1, 3],
    ]);
  });

  it("the send_message limit counts in SQLite on the key of the target run", async () => {
    const { token, runId } = await oneRun();
    const detail = await withFx(token, (_fx, sql) => plan(sql, SENT_MESSAGES_SQL, runId, 0, "run:x"));
    expect(detail).toMatch(/SEARCH run_messages USING (INDEX|PRIMARY KEY)/);
  });
});

describe("listRunSessions", () => {
  it("reads the snapshot parts of all sessions in one query", () => {
    const now = "2026-01-01T00:00:00.000Z";
    const sessions = [1, 2, 3].map(
      (n) =>
        ({
          ...activeSession("run_x"),
          session: n,
          state: "ended",
          assigned_at: now,
        }) as RunSession,
    );
    const queries: string[] = [];
    const sql = {
      exec(q: string) {
        queries.push(q);
        return {
          toArray: () => [
            { session: 1, part: "workspace" },
            { session: 3, part: "workspace" },
            { session: 3, part: "harness_state" },
          ],
        };
      },
    } as unknown as SqlStorage;
    const views = sessionViews(sessions, new Repo(sql, Date.now).snapshotPartsBySession("run_x"));
    expect(queries).toHaveLength(1);
    expect(views.map((v) => [v.number, v.snapshot])).toEqual([
      [1, { workspace: true, harness_state: false }],
      [2, { workspace: false, harness_state: false }],
      [3, { workspace: true, harness_state: true }],
    ]);
  });
});

describe("runner event batches", () => {
  it("write the session once per batch", async () => {
    const { token, runId } = await oneRun();
    await withFx(token, (fx) => fx.repo.createSession(activeSession(runId)));
    const before = await withFx(token, (fx) => fx.repo.getSession(runId, 1)!.version);
    const msg = (seq: number) => ({ seq, type: "message", data: { role: "assistant", content: `m${seq}` } });
    const usage = (seq: number, cost: number) => ({ seq, type: "usage", data: { cumulative: true, cost_usd: cost } });
    const res = await workStub(token).runnerEvents(runId, 1, {
      events: [msg(1), usage(2, 0.1), msg(3), usage(4, 0.25), msg(5)],
    });
    expect(res).toEqual({ ack: 5 });
    const after = await withFx(token, (fx) => ({ s: fx.repo.getSession(runId, 1)!, run: fx.repo.mustRun(runId) }));
    // One write for five events (before: one for the ack and one more per usage event).
    expect(after.s.version - before).toBe(1);
    expect(after.s.event_ack).toBe(5);
    expect(after.s.usage.cost_usd).toBe(0.25);
    expect(after.run.usage.cost_usd).toBe(0.25);
  });

  it("write the session before a turn transition reads it", async () => {
    const { token, runId } = await oneRun();
    await withFx(token, (fx) => fx.repo.createSession(activeSession(runId)));
    await workStub(token).runnerEvents(runId, 1, {
      events: [
        { seq: 1, type: "turn.started", turn_id: "t1" },
        { seq: 2, type: "usage", data: { cumulative: true, cost_usd: 0.5 } },
      ],
    });
    let s = await withFx(token, (fx) => fx.repo.getSession(runId, 1)!);
    expect(s.active_turn_id).toBe("t1");
    expect(s.usage.cost_usd).toBe(0.5);
    await workStub(token).runnerEvents(runId, 1, {
      events: [{ seq: 3, type: "turn.ended", turn_id: "t1", data: { outcome: "done", result: "done" } }],
    });
    s = await withFx(token, (fx) => fx.repo.getSession(runId, 1)!);
    const run = await withFx(token, (fx) => fx.repo.mustRun(runId));
    expect(s.active_turn_id).toBe("");
    expect(s.event_ack).toBe(3);
    expect(run.turns).toBe(1);
    expect(run.result).toBe("done");
  });
});

describe("retentionSweep", () => {
  it("reads finished runs one page at a time and sets the timer for the next expiry", async () => {
    const f = await buildRuns(
      {},
      {
        a: { state: "succeeded" },
        b: { state: "failed" },
        c: { state: "cancelled" },
        d: { state: "succeeded" },
        e: { state: "error" },
        fresh: { state: "succeeded" },
        live: { state: "running" },
      },
    );
    const keep = 3_600_000;
    const old = new Date(Date.now() - 2 * keep).toISOString();
    const recent = Date.now() - 1000;
    const expired = ["a", "b", "c", "d", "e"].map((n) => f.ids[n]!);
    await withFx(f.token, (fx) => {
      for (const id of expired) fx.repo.putRun(Object.assign(fx.repo.mustRun(id), { finished_at: old }));
      fx.repo.putRun(Object.assign(fx.repo.mustRun(f.ids.fresh!), { finished_at: new Date(recent).toISOString() }));
    });
    for (const id of [...expired, f.ids.fresh!]) await env.DATA.put(`runs/${id}/transcript.jsonl`, "x");

    const out = await runInDurableObject(workStub(f.token) as unknown as DurableObjectStub, async (inst, state) => {
      const host = (inst as unknown as { host(): WorkHost }).host();
      const steps: boolean[] = [];
      for (let i = 0; i < 10; i++) {
        const r = await retentionStep(host, keep, 2);
        steps.push(r.more);
        if (!r.more) break;
      }
      const pruned = state.storage.sql
        .exec<{ run_id: string }>("SELECT run_id FROM run_state WHERE k = 'data_pruned' ORDER BY run_id")
        .toArray()
        .map((r) => r.run_id);
      const timer = state.storage.sql
        .exec<{ due_ms: number }>("SELECT due_ms FROM timers WHERE name = 'retention'")
        .toArray()[0]?.due_ms;
      return { steps, pruned, timer };
    });
    // Six finished runs in pages of two: three full pages, then an empty one ends the sweep.
    expect(out.steps).toEqual([true, true, true, false]);
    expect(out.pruned).toEqual([...expired].sort());
    for (const id of expired) expect(await env.DATA.head(`runs/${id}/transcript.jsonl`)).toBeNull();
    expect(await env.DATA.head(`runs/${f.ids.fresh!}/transcript.jsonl`)).not.toBeNull();
    expect(out.timer).toBeLessThanOrEqual(recent + keep + 60_000);
    expect(out.timer).toBeGreaterThan(recent + keep);
  });
});
