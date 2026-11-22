// Golden and table tests of the pure engine parts (ports of the Go cases in
// lifecycle_test.go, dispatch_test.go, timeline_test.go, liveness and
// deadline tests, spend_test.go, idempotency_test.go).
import { describe, expect, it } from "vitest";
import { admit, refill, type Bucket, type WaitingRow } from "./admission";
import { canonicalJson, toolIdempotencyKey } from "./canonical";
import { decideDelivery, messageStartsTurn, outcomeFor } from "./deliver";
import { requiredHosts } from "./hosts";
import { lifecycleDedupeKey, lifecycleEvent, lifecycleParts, truncateUTF8, turnMarker } from "./lifecycle";
import { decideDeadline, decideSilent } from "./liveness";
import { computeSeconds, parseDuration, priorRetries, setFailureMessage } from "./sessions";
import { queuedEventId, transitionRows } from "./timeline";
import { buildDispatchPrompt, truncate, workItemTitle } from "./text";
import type { Run } from "./types";
import { buildLimits, dayKey, exceeded, monthKey, warnings } from "../spend/spend";
import { mintSessionToken, runnerKeys, verifySessionToken } from "../runner-api/token";

const run = (over: Partial<Run> = {}): Run =>
  ({
    id: "run_c",
    version: 1,
    team_id: "t",
    factory_id: "f",
    state: "succeeded",
    prompt: "p",
    config: {} as Run["config"],
    consumed_seq: 0,
    usage: { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 },
    created_at: "2026-11-22T10:00:00.000Z",
    ...over,
  }) as Run;

describe("lifecycle", () => {
  it("maps states to events", () => {
    expect(["succeeded", "blocked", "failed", "error", "cancelled", "running", "queued"].map(lifecycleEvent)).toEqual([
      "succeeded",
      "blocked",
      "failed",
      "error",
      "cancelled",
      "",
      "",
    ]);
  });
  it("text and data of a failed child", () => {
    const p = lifecycleParts(
      run({ name: "implement", agent_name: "coder", state: "failed", failure_code: "agent_failed", result: "x" }),
      false,
      "failed",
      "https://f.example",
    );
    expect(p.content).toBe('Child run "implement" failed. Failure code: agent_failed.');
    expect(Object.keys(p.data)).toEqual([
      "event",
      "run_id",
      "name",
      "agent",
      "failure_code",
      "result",
      "result_truncated",
      "url",
    ]);
    expect(lifecycleParts(run({ name: "" }), true, "succeeded", "").content).toBe('Side run "run_c" succeeded.');
  });
  it("truncates the result on a UTF-8 boundary", () => {
    const [s, cut] = truncateUTF8("é".repeat(4000), 6144);
    expect(cut).toBe(true);
    expect(new TextEncoder().encode(s).length).toBeLessThanOrEqual(6144);
    expect(s.endsWith("é")).toBe(true);
  });
  it("dedupe keys and turn markers", () => {
    expect(lifecycleDedupeKey(run({ turns: 2 }), "succeeded")).toBe("lifecycle:run_c:succeeded:2");
    expect(lifecycleDedupeKey(run({ turns: 2 }), "started")).toBe("lifecycle:run_c:started");
    expect(turnMarker(run({ turns: 3, consumed_seq: 5 }))).toBe("5:3");
    expect(turnMarker(run({ turns: 3, state: "cancelled" }))).toBe("0:3:cancelled");
  });
});

describe("delivery", () => {
  it("system and lifecycle started do not start a turn", () => {
    expect(messageStartsTurn({ kind: "system" })).toBe(false);
    expect(messageStartsTurn({ kind: "lifecycle", data: { event: "started" } })).toBe(false);
    expect(messageStartsTurn({ kind: "lifecycle", data: { event: "succeeded" } })).toBe(true);
    expect(messageStartsTurn({ kind: "user" })).toBe(true);
  });
  it("outcomes of Send", () => {
    expect(outcomeFor("succeeded", { state: "active" })).toBe("live_session");
    expect(outcomeFor("running", undefined)).toBe("queued");
    expect(outcomeFor("succeeded", { state: "ended" })).toBe("new_session");
  });
  it("decides start, mid-turn and wait", () => {
    const ready = { state: "active" as const, ready_at: "x", capabilities: [] as string[] };
    expect(decideDelivery({ ...ready, active_turn_id: "" }, [{ kind: "user" }]).kind).toBe("start_turn");
    expect(decideDelivery({ ...ready, active_turn_id: "t" }, [{ kind: "user" }]).kind).toBe("none");
    expect(
      decideDelivery({ ...ready, active_turn_id: "t", capabilities: ["mid_turn_input"] }, [{ kind: "user" }]).kind,
    ).toBe("mid_turn");
    expect(decideDelivery({ ...ready, ready_at: undefined, active_turn_id: "" }, [{ kind: "user" }]).kind).toBe("none");
  });
});

describe("sessions", () => {
  it("priorRetries counts lost and not_started sessions before, newest first", () => {
    const s = (session: number, end_reason: string) => ({ session, state: "ended" as const, end_reason });
    expect(priorRetries([s(1, "idle"), s(2, "lost"), s(3, "not_started"), s(4, "lost")], 4)).toBe(2);
    expect(priorRetries([s(1, "lost")], 2)).toBe(1);
    expect(priorRetries([], 1)).toBe(0);
  });
  it("setFailureMessage only for a bare code", () => {
    const r = run({ state: "failed", failure_code: "agent_failed", error: "agent_failed" });
    setFailureMessage(r, "  boom  ");
    expect(r.error).toBe("agent_failed: boom");
    setFailureMessage(r, "again");
    expect(r.error).toBe("agent_failed: boom");
  });
  it("computeSeconds from start or assignment", () => {
    expect(computeSeconds({ assigned_at: "2026-01-01T00:00:00Z" }, Date.parse("2026-01-01T00:00:10Z"))).toBe(10);
    expect(
      computeSeconds(
        { assigned_at: "2026-01-01T00:00:00Z", started_at: "2026-01-01T00:00:04Z" },
        Date.parse("2026-01-01T00:00:10Z"),
      ),
    ).toBe(6);
  });
  it("parses the duration vars", () => {
    expect(parseDuration("30m", 0)).toBe(1_800_000);
    expect(parseDuration("4h", 0)).toBe(14_400_000);
    expect(parseDuration("1h30m", 0)).toBe(5_400_000);
    expect(parseDuration("bogus", 7)).toBe(7);
  });
});

describe("timeline", () => {
  it("keeps the Go event ID formulas", () => {
    const r = run({ state: "queued", queued_at: "2026-11-22T10:00:00.000Z" });
    expect(queuedEventId(r)).toBe(`run.queued:run_c:${Date.parse("2026-11-22T10:00:00.000Z")}000000`);
    const rows = transitionRows(
      r,
      { event: "session_lost", failureCode: "sandbox_lost", priorRetries: 1 },
      { state: "queued", changed: true, retry: true, failureCode: "sandbox_lost" },
      2,
      0,
    );
    expect(rows.map((x) => x.type)).toEqual(["run.retry_scheduled", "run.queued"]);
    expect(rows[0]!.data).toEqual({ failure_code: "sandbox_lost", attempt: 2 });
    expect(
      transitionRows(run({ state: "cancelled" }), { event: "cancel" }, { state: "cancelled", changed: true }, 1, 0),
    ).toEqual([]);
  });
});

describe("liveness and deadline", () => {
  const lostAfterMs = 90_000;
  const base = { state: "active" as const, bootstrap_expires_at: "2099-01-01T00:00:00Z" };
  it("asks the provider after the silence, then decides", () => {
    const now = Date.parse("2026-01-01T00:10:00Z");
    const s = { ...base, last_seen_at: new Date(now - 100_000).toISOString() };
    expect(decideSilent(s, now, { lostAfterMs }).kind).toBe("ask_status");
    expect(decideSilent(s, now, { lostAfterMs }, "not_found")).toMatchObject({ kind: "lose", code: "sandbox_lost" });
    expect(
      decideSilent(s, now, { lostAfterMs }, { taskId: "x", state: "exited", reason: "container_oom" }),
    ).toMatchObject({ kind: "lose", useReasonMap: true, reason: "container_oom" });
    expect(decideSilent(s, now, { lostAfterMs }, { taskId: "x", state: "running" }).kind).toBe("wait");
    const s2 = { ...base, last_seen_at: new Date(now - 200_000).toISOString() };
    expect(decideSilent(s2, now, { lostAfterMs }, "unavailable")).toMatchObject({
      kind: "lose",
      code: "runner_lost",
      cancel: true,
    });
  });
  it("an assigned session is lost only on exit, not found or the start timeout", () => {
    const now = Date.parse("2026-01-01T00:10:00Z");
    const s = {
      state: "assigned" as const,
      bootstrap_expires_at: "2099-01-01T00:00:00Z",
      last_seen_at: new Date(now - 500_000).toISOString(),
    };
    expect(decideSilent(s, now, { lostAfterMs }, { taskId: "x", state: "running" }).kind).toBe("wait");
    expect(
      decideSilent({ ...s, bootstrap_expires_at: new Date(now - 1).toISOString() }, now, { lostAfterMs }),
    ).toMatchObject({ code: "sandbox_create_failed" });
  });
  it("deadline: stop once, then lose after the grace", () => {
    const o = { maxSessionMs: 1000, stopGraceMs: 100, lostAfterMs: 90 };
    const s = { state: "active" as const, assigned_at: new Date(0).toISOString(), active_turn_id: "t" };
    expect(decideDeadline(s, undefined, 500, o).kind).toBe("wait");
    expect(decideDeadline(s, undefined, 1500, o)).toEqual({ kind: "stop", applyDeadlineEvent: true });
    expect(decideDeadline(s, 1500, 1100, o).kind).toBe("wait");
    expect(decideDeadline(s, 1500, 1300, o)).toEqual({ kind: "lose", applyEvent: true });
  });
});

describe("tool idempotency key (rule 3)", () => {
  it("does not depend on key order", async () => {
    const a = await toolIdempotencyKey("r", "t", "run_agents", { b: 1, a: [{ y: 2, x: 1 }] });
    const b = await toolIdempotencyKey("r", "t", "run_agents", { a: [{ x: 1, y: 2 }], b: 1 });
    expect(a).toBe(b);
    expect(a.startsWith("orch:idem:r:t:run_agents:")).toBe(true);
    expect(canonicalJson({ b: undefined, a: 1 })).toBe('{"a":1}');
  });
});

describe("admission", () => {
  const row = (run_id: string, factory_id: string, target_key = "provider:local"): WaitingRow => ({
    run_id,
    session: 1,
    factory_id,
    target: { kind: "provider", providerId: target_key.slice(9) },
    target_key,
    hold: "",
  });
  it("factory limit is exact and FIFO", () => {
    const res = admit(
      {
        waiting: [row("a", "f"), row("b", "f"), row("c", "g")],
        leases: [],
        factoryLimits: new Map([["f", 1]]),
        workers: [],
        buckets: new Map(),
        spendBlocked: new Set(),
        maxSandboxes: 0,
      },
      0,
    );
    expect(res.grants.map((g) => g.run_id)).toEqual(["a", "c"]);
  });
  it("a worker target needs a free slot; a pool picks the least used worker", () => {
    const w = { online: true, draining: false, used: 0, factories: [] as string[] };
    const res = admit(
      {
        waiting: [
          { ...row("a", "f"), target: { kind: "pool", label: "gpu" }, target_key: "pool:gpu" },
          { ...row("b", "f"), target: { kind: "pool", label: "gpu" }, target_key: "pool:gpu" },
          { ...row("c", "f"), target: { kind: "worker", workerId: "w1" }, target_key: "worker:w1" },
        ],
        leases: [],
        factoryLimits: new Map(),
        workers: [
          { ...w, worker_id: "w1", slots: 1, labels: { pool: "gpu" } },
          { ...w, worker_id: "w2", slots: 1, labels: { pool: "gpu" } },
        ],
        buckets: new Map(),
        spendBlocked: new Set(),
        maxSandboxes: 0,
      },
      0,
    );
    expect(res.grants.map((g) => [g.run_id, g.worker_id])).toEqual([
      ["a", "w1"],
      ["b", "w2"],
    ]);
  });
  it("refills token buckets", () => {
    const b: Bucket = { provider: "boatdev", window_s: 60, tokens: 0, cap: 12, updated_ms: 0 };
    expect(refill(b, 5000).tokens).toBeCloseTo(1);
  });
});

describe("spend", () => {
  it("limits, exceeded and warnings", () => {
    const now = Date.parse("2026-11-22T12:00:00Z");
    const l = buildLimits("factory", { daily: 10, monthly: 100 }, 8.5, 20, now);
    expect(exceeded(l)).toBeUndefined();
    expect(warnings(l).map((x) => x.period)).toEqual(["daily"]);
    expect(exceeded(buildLimits("factory", { daily: 10 }, 10, 0, now))?.period).toBe("daily");
    expect(dayKey(now)).toBe("2026-11-22");
    expect(monthKey(now)).toBe("2026-11");
  });
});

describe("text and hosts", () => {
  it("titles and prompts", () => {
    expect(workItemTitle({ prompt: "\n first line \nsecond" })).toBe("first line");
    expect(workItemTitle({ title: "x".repeat(130) })).toHaveLength(120);
    expect(buildDispatchPrompt({ prompt: "do it" })).toBe("do it");
    expect(truncate("abcdef", 3)).toBe("abc\n…[truncated 3 bytes]");
  });
  it("required hosts add the control plane and the R2 S3 host", () => {
    const h = requiredHosts(
      { harness: { type: "claude" } },
      { controlUrl: "https://factory.example.com", r2AccountId: "ABC123" },
    );
    expect(h).toEqual(["factory.example.com", "abc123.r2.cloudflarestorage.com", "api.anthropic.com"]);
  });
});

describe("session tokens", () => {
  it("mints and verifies; refuses a tampered or expired token", async () => {
    const keys = await runnerKeys({ OF_SIGNING_KEY: "test-only-signing-key-0123456789abcdef" });
    const { token } = await mintSessionToken(keys, { team: "default", run: "run_x", ses: 2, sub: "runner" }, 1_000_000);
    expect(token.startsWith("ofs_")).toBe(true);
    expect((await verifySessionToken(keys, token, 1_000_000))?.ses).toBe(2);
    expect(await verifySessionToken(keys, token + "x", 1_000_000)).toBeUndefined();
    expect(await verifySessionToken(keys, token, 1_000_000 + 3_700_000)).toBeUndefined();
    const other = await runnerKeys({ OF_SIGNING_KEY: "another-test-only-key-0123456789abcdef" });
    expect(await verifySessionToken(other, token, 1_000_000)).toBeUndefined();
  });
});
