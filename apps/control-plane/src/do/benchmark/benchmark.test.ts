// BenchmarkDO: port of the benchmark tests of internal/scoring
// (benchmark_test.go, judge_test.go) onto the DO.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, newRoutedId, newToken, type BenchmarkSuite, type HarnessType } from "@openfactory/core";
import { describe, expect, it, vi } from "vitest";
import type { CallMeta } from "../../contracts/do-messages";
import { CORRECTNESS_SCORER, type BenchmarkResults, type BenchmarkScorer } from "../../scoring/benchmark";
import { FakeWorld, finishJudge, run } from "../../scoring/testkit";
import type { BenchmarkDO } from "./index";
import { DISPATCH_CHUNK, RECONCILE_EVERY_MS, type BenchmarkPlan, type BenchmarkRunJson } from "./index";

const SHA = "0123456789abcdef0123456789abcdef01234567";
const editor: CallMeta = {
  requestId: "t",
  principal: { kind: "user", id: "u1", userId: "u1", teamId: "default", role: "editor", factoryRoles: {} },
};
const reader: CallMeta = {
  requestId: "t",
  principal: { kind: "user", id: "u2", userId: "u2", teamId: "default", role: "user", factoryRoles: {} },
};

const suite: BenchmarkSuite = {
  slug: "fix",
  name: "Fix",
  agent: "implement",
  configurations: [],
  tasks: [
    {
      slug: "broken-link",
      title: "Fix link",
      prompt: "Fix the broken link.",
      successCriteria: "The link resolves.",
      startingRepoRefs: [
        { codeForge: "GITHUB", owner: "acme", repo: "app", ref: SHA },
        { codeForge: "GITHUB", owner: "acme", repo: "docs", ref: SHA },
      ],
    },
    { slug: "typo", title: "Fix typo", prompt: "Fix the typo.", successCriteria: "Typo gone." },
  ],
};

const testsRun: BenchmarkScorer = {
  key: "tests-run",
  scorer_id: "sc_tests",
  name: "tests-run",
  model: "judge-model",
  rubric: "Did the agent run tests?",
  labels: [
    { value: "tests_run", score: 1 },
    { value: "tests_skipped", score: 0 },
  ],
  passing_score: 1,
  harness: { type: "claude", model: "judge-model" },
};

function plan(over: Partial<BenchmarkPlan> = {}): BenchmarkPlan {
  return {
    suite,
    configurations: [
      { name: "Baseline", agents: [{ agent: "implement", model: "model-big" }] },
      {
        name: "Candidate",
        agents: [
          // "claude-code" is an alias that the dispatch normalizes to "claude".
          { agent: "implement", harness: { type: "claude-code" as HarnessType, model: "model-small" } },
          { agent: "review", model: "model-review" },
        ],
      },
    ],
    repetitions: 1,
    correctness_model: "claude-sonnet-4-5",
    correctness_judge: { harness: { type: "claude", model: "judge" } },
    agents: ["implement", "review"],
    ...over,
  };
}

async function setup(p: BenchmarkPlan = plan(), scorers: BenchmarkScorer[] = []) {
  const world = new FakeWorld();
  const token = newToken("b");
  const stub = env.BENCHMARK.getByName(doName.benchmark("default", token));
  const id = newRoutedId("bench", token);
  const call = <T>(fn: (inst: BenchmarkDO) => Promise<T>): Promise<T> =>
    runInDurableObject(stub, async (inst: BenchmarkDO) => {
      inst.deps = { runs: world, engine: world };
      inst.clock = world.clock.now;
      return fn(inst);
    });
  const start = (over: Partial<{ plan: BenchmarkPlan }> = {}) =>
    call((i) =>
      i.start(editor, {
        id,
        factoryId: "fac_1",
        plan: { ...(over.plan ?? p), ...(scorers.length ? { scorers } : {}) },
        repoSubstitutions: [
          {
            source: { codeForge: "GITHUB", owner: "acme", name: "app" },
            target: { codeForge: "GITHUB", owner: "acme-bench", name: "app-fork" },
          },
        ],
      }),
    );
  const get = () => call((i) => i.get(editor));
  /** Waits until the alarm steps have dispatched every trial. */
  const dispatchAll = () =>
    vi.waitFor(async () => expect((await get()).results.trials.every((x) => x.status !== "pending")).toBe(true), {
      timeout: 5000,
      interval: 20,
    });
  const finishTrial = (
    runId: string,
    state: "succeeded" | "failed" | "idle",
    msg: string,
    cost: number,
    durMin: number,
  ) => {
    const r = world.runsById.get(runId)!;
    r.state = state;
    r.finished_at = new Date(Date.parse(r.created_at) + durMin * 60_000).toISOString();
    r.usage = { input_tokens: 100, output_tokens: 10, cost_usd: cost, duration_ms: 0 };
    world.say(runId, "assistant", msg);
    return r;
  };
  /** Finishes every queued judge run: correctness by "fixed" in the transcript, tests-run by "go test". */
  const settleJudges = async () => {
    for (const j of [...world.judges]) {
      const r = world.runsById.get(j.runId)!;
      if (r.state !== "queued") continue;
      const tr = j.req.files["transcript.md"] ?? "";
      const label = j.req.name.includes("correctness")
        ? tr.includes("fixed")
          ? "correct"
          : "incorrect"
        : tr.includes("go test")
          ? "tests_run"
          : "tests_skipped";
      finishJudge(world, j.runId, "succeeded", `{"label": "${label}", "rationale": "r"}`, 0.1);
      await call((i) => i.trialJudgeUpdate(j.runId));
    }
  };
  return { world, stub, id, call, start, get, dispatchAll, finishTrial, settleJudges };
}

describe("BenchmarkDO", () => {
  it("dispatches trials in chunks and runs the lifecycle", async () => {
    const t = await setup(plan({ repetitions: 2 }), [testsRun]);
    t.world.hold(DISPATCH_CHUNK);
    const started = await t.start();
    expect(started.state).toBe("running");
    expect(started.results.trials).toHaveLength(8); // 2 configs x 2 tasks x 2 reps
    expect(started.results.trials.every((x) => x.status === "pending")).toBe(true);
    // One alarm step dispatches one chunk, then reschedules itself.
    await vi.waitFor(() => expect(t.world.inFlight).toBeGreaterThan(0), { timeout: 3000, interval: 10 });
    expect(t.world.dispatches).toHaveLength(DISPATCH_CHUNK);
    t.world.releaseHold();
    await t.dispatchAll();
    expect(t.world.dispatches).toHaveLength(8);
    // The same trial is never dispatched twice, and each dispatch carries its trial ID as the key.
    expect(new Set(t.world.dispatches.map((d) => d.key)).size).toBe(8);

    for (const { req } of t.world.dispatches) {
      expect(req).toMatchObject({
        agentName: "implement",
        triggeredBy: `benchmark:${t.id}`,
        source: { provider: "benchmark" },
      });
      expect(req.metadata).toMatchObject({ benchmark_run: t.id, trial_mode: "true" });
      if (req.metadata!["benchmark_configuration"] === "Baseline") {
        expect(req.overrides).toEqual({ model: "model-big" });
      } else {
        expect(req.overrides).toEqual({ harness: { type: "claude", model: "model-small" } });
        expect(req.agentPins).toEqual({ review: { model: "model-review" } });
      }
      if (req.metadata!["benchmark_task"] === "broken-link") {
        // benchmarkRepoSubstitutions: acme/app -> acme-bench/app-fork; acme/docs unchanged.
        expect(req.startingRepoRefs).toEqual([
          { codeForge: "GITHUB", owner: "acme-bench", repo: "app-fork", ref: SHA },
          { codeForge: "GITHUB", owner: "acme", repo: "docs", ref: SHA },
        ]);
        expect(req.prompt).toContain("github.com:acme-bench/app-fork@" + SHA);
      }
    }

    const trials = (await t.get()).results.trials;
    expect(trials.every((x) => x.status === "running" && x.run_id)).toBe(true);
    for (let i = 0; i < trials.length; i++) {
      const tr = trials[i]!;
      let msg = "fixed it; ran go test";
      let cost = 2;
      let dur = 10;
      if (tr.configuration === "Candidate") {
        msg = tr.repetition === 1 ? "fixed quickly" : "attempted";
        cost = 0.5;
        dur = 2;
      }
      if (i === 0) {
        // A descendant (it inherits the trial metadata) counts toward the cost and its own update is ignored.
        const child = t.world.put(
          run({
            id: "child_of_trial",
            parent_run_id: tr.run_id!,
            agent_name: "review",
            metadata: { benchmark_run: t.id, benchmark_trial: tr.id },
            usage: { cost_usd: 1, input_tokens: 5, output_tokens: 5, duration_ms: 0 },
          }),
        );
        await t.call((x) => x.trialRunUpdate(child.id));
      }
      const r = t.finishTrial(tr.run_id!, "succeeded", msg, cost, dur);
      await t.call((x) => x.trialRunUpdate(r.id));
      await t.call((x) => x.trialRunUpdate(r.id)); // re-delivery is a no-op
      if (i < trials.length - 1) expect((await t.get()).state).toBe("running");
    }
    expect(t.world.judges).toHaveLength(16); // 8 trials x (correctness + tests-run)
    expect((await t.get()).state).toBe("scoring");
    await t.settleJudges();
    const b = await t.get();
    expect(b.state).toBe("complete");
    expect(b.finished_at).toBeTruthy();
    const res = b.results;
    const [base, cand] = res.summary;
    expect(base).toMatchObject({
      name: "Baseline",
      trials: 4,
      scored: 4,
      passed: 4,
      pass_rate: 1,
      median_duration_ms: 600_000,
      input_tokens: 405,
    });
    expect(base!.total_cost_usd).toBeCloseTo(9);
    expect(base!.avg_cost_usd).toBeCloseTo(2.25);
    expect(base!.scorer_averages).toMatchObject({ [CORRECTNESS_SCORER]: 1, "tests-run": 1 });
    expect(cand).toMatchObject({ passed: 2, pass_rate: 0.5 });
    expect(cand!.avg_cost_usd).toBeCloseTo(0.5);
    expect(cand!.overall).toBeCloseTo(0.25);
    expect(res.recommendations).toEqual({ overall: "Baseline", lowest_cost: "Candidate", most_efficient: "Candidate" });
    // 8 trials (9 + 2 for Baseline and Candidate) + the child + 16 judges at 0.1.
    expect(res.total_cost_usd).toBeCloseTo(11 + 1.6);
    expect(res.judge_cost_usd).toBeCloseTo(1.6);
    // Succeeded trials are released after scoring.
    expect(t.world.cancels.filter((c) => c.endsWith("by benchmark:" + t.id))).toHaveLength(8);
  });

  it("is idempotent on start and projects the summary to IndexDO", async () => {
    const t = await setup();
    const a = await t.start();
    const b = await t.start();
    expect(b.id).toBe(a.id);
    expect(b.results.trials.map((x) => x.id)).toEqual(a.results.trials.map((x) => x.id));
    const rows = await t.call(async (i) => {
      const sql = (i as unknown as { sql: SqlStorage }).sql;
      return sql
        .exec<{ kind: string; msg_id: string }>("SELECT kind, msg_id FROM outbox WHERE kind = 'index.project'")
        .toArray();
    });
    expect(rows.length).toBeGreaterThanOrEqual(1);
    expect(rows[0]!.msg_id).toBe(`bench:${t.id}:1`);
  });

  it("schedules its timers on the real clock, not on the clock of the trial timestamps", async () => {
    // The test clock is months in the past. A reconcile timer due on it fires at
    // once and re-arms itself in the past again: a busy loop of alarms.
    const t = await setup(plan({ configurations: [{ name: "only" }] }));
    let gets = 0;
    const getRun = t.world.getRun.bind(t.world);
    t.world.getRun = (id: string) => {
      gets++;
      return getRun(id);
    };
    const before = Date.now();
    await t.start();
    await t.dispatchAll();
    const due = await t.call(async (i) => {
      const sql = (i as unknown as { sql: SqlStorage }).sql;
      return sql.exec<{ due_ms: number }>("SELECT due_ms FROM timers WHERE name = 'bench.reconcile'").one().due_ms;
    });
    expect(due).toBeGreaterThanOrEqual(before + RECONCILE_EVERY_MS);
    // Nothing finished, so nothing reads a run until the reconcile check is due.
    await new Promise((r) => setTimeout(r, 200));
    expect(gets).toBe(0);
  });

  it("requires the editor role to start and the user role to read", async () => {
    const t = await setup();
    await expect(t.call((i) => i.start(reader, { id: t.id, factoryId: "fac_1", plan: plan() }))).rejects.toThrow(
      /editor/,
    );
    await t.start();
    expect((await t.call((i) => i.get(reader))).id).toBe(t.id);
    await expect(t.call((i) => i.cancel(reader))).rejects.toThrow(/editor/);
  });

  it("marks a blocked trial, scores it, and reconciles a dropped update", async () => {
    const t = await setup(plan({ configurations: [{ name: "only" }] }));
    await t.start();
    await t.dispatchAll();
    const trials = (await t.get()).results.trials;
    expect(trials).toHaveLength(2);
    // Trial 1 goes idle waiting on a human: blocked, still scored, then released.
    const r1 = t.world.runsById.get(trials[0]!.run_id!)!;
    t.finishTrial(r1.id, "idle", "fixed, awaiting approval", 0, 1);
    t.world.inboxOpen.push({ id: "in_1", run_id: r1.id });
    // Trial 2 finishes but its update is "dropped": the reconcile timer picks it up.
    t.finishTrial(trials[1]!.run_id!, "failed", "could not do it", 0.1, 1);
    await t.call((i) => i.trialRunUpdate(r1.id));
    await t.settleJudges();
    await t.call(async (i) => {
      // Fire the reconcile timer now.
      const x = i as unknown as { tx(f: () => void): void; setTimer(n: string, k: string, d: number): void };
      x.tx(() => x.setTimer("bench.reconcile", "bench.reconcile", Date.now() - 1000));
    });
    await vi.waitFor(() => expect(t.world.judges.length).toBeGreaterThanOrEqual(2), { timeout: 5000, interval: 20 });
    await t.settleJudges();
    const b = await t.get();
    expect(b.state).toBe("complete");
    const [x, y] = b.results.trials;
    expect(x).toMatchObject({ status: "blocked", scored: true });
    expect(x!.scores![0]!.passed).toBe(true);
    expect(y).toMatchObject({ status: "finished", run_state: "failed", scored: true });
    expect(y!.scores![0]!.passed).toBe(false);
    expect(b.results.summary[0]).toMatchObject({ blocked: 1, pass_rate: 0.5 });
    expect(t.world.cancels.filter((c) => c.startsWith(r1.id))).toHaveLength(1);
  });

  it("adopts a trial run that finishes before the dispatch step recorded it", async () => {
    const t = await setup(plan({ configurations: [{ name: "only" }] }));
    const started = await t.start();
    const tr = started.results.trials[0]!;
    // The run exists (idempotent dispatch) and ends before the DO commits the dispatch.
    const r = t.world.put(
      run({
        id: "run_early",
        agent_name: "implement",
        state: "succeeded",
        metadata: { benchmark_run: t.id, benchmark_trial: tr.id },
        finished_at: t.world.clock.iso(),
      }),
    );
    t.world.say(r.id, "assistant", "fixed");
    await t.call((i) => i.trialRunUpdate(r.id));
    const trial = (await t.get()).results.trials[0]!;
    expect(trial).toMatchObject({ run_id: "run_early", status: "finished" });
    expect(t.world.judges.length).toBeGreaterThan(0);
  });

  it("cancels: pending trials at once, runs in chunks, late results ignored", async () => {
    const t = await setup(
      plan({
        configurations: [{ name: "a" }, { name: "b" }, { name: "c" }],
        suite: { ...suite, tasks: suite.tasks!.map((x) => ({ ...x })) },
      }),
    );
    t.world.hold(DISPATCH_CHUNK);
    await t.start();
    await vi.waitFor(() => expect(t.world.inFlight).toBeGreaterThan(0), { timeout: 3000, interval: 10 });
    expect(t.world.dispatches).toHaveLength(DISPATCH_CHUNK);
    const mid = (await t.get()).results.trials;
    const done = mid.find((x) => x.status === "running")!;
    const r = t.finishTrial(done.run_id!, "succeeded", "fixed", 1, 1);
    await t.call((i) => i.trialRunUpdate(r.id));
    await t.settleJudges();
    const cancelled = await t.call((i) => i.cancel(editor));
    expect(cancelled.state).toBe("cancelled");
    expect(cancelled.finished_at).toBeTruthy();
    expect(cancelled.results.cancelled_by).toBe("user:u1");
    // The dispatch that was in flight finishes; its run is cancelled too and no more are started.
    t.world.releaseHold();
    const running = mid.filter((x) => x.status === "running" && x.id !== done.id).map((x) => x.run_id!);
    await vi.waitFor(
      () => {
        for (const id of running) expect(t.world.cancels.some((c) => c.startsWith(id))).toBe(true);
      },
      { timeout: 5000, interval: 20 },
    );
    await vi.waitFor(() => expect(t.world.inFlight).toBe(0), { timeout: 5000 });
    expect(t.world.dispatches.length).toBeLessThanOrEqual(DISPATCH_CHUNK + 1);
    // A late completion changes nothing; the scored trial is kept.
    const late = running[0]!;
    t.finishTrial(late, "succeeded", "fixed", 1, 1);
    await t.call((i) => i.trialRunUpdate(late));
    const after = await t.get();
    expect(after.state).toBe("cancelled");
    const kept = after.results.trials.find((x) => x.id === done.id)!;
    expect(kept.scored).toBe(true);
    expect(after.results.trials.filter((x) => x.scored)).toHaveLength(1);
    expect(after.results.summary.reduce((n, s) => n + s.scored, 0)).toBe(1);
    // Cancelling again is idempotent.
    expect((await t.call((i) => i.cancel(editor))).state).toBe("cancelled");
  });

  it("stops the judge runs of a cancelled benchmark", async () => {
    const t = await setup(plan({ configurations: [{ name: "only" }], suite: { ...suite, tasks: [suite.tasks![1]!] } }));
    await t.start();
    await t.dispatchAll();
    const tr = (await t.get()).results.trials[0]!;
    const r = t.finishTrial(tr.run_id!, "succeeded", "fixed", 1, 1);
    await t.call((i) => i.trialRunUpdate(r.id));
    expect(t.world.judges).toHaveLength(1);
    const view = await t.call((i) => i.cancel(editor));
    expect(view.results.trials[0]!.scores![0]).toMatchObject({ pending: false, error: "cancelled" });
    await vi.waitFor(() => expect(t.world.cancels.some((c) => c.startsWith(t.world.judges[0]!.runId))).toBe(true), {
      timeout: 5000,
      interval: 20,
    });
  });

  it("fails when no trial can be dispatched", async () => {
    const t = await setup(plan({ configurations: [{ name: "only" }], suite: { ...suite, tasks: [suite.tasks![1]!] } }));
    t.world.failDispatchFor = "implement";
    await t.start();
    await t.dispatchAll();
    const b = await t.get();
    expect(b.state).toBe("failed");
    expect(b.finished_at).toBeTruthy();
    expect(b.results.trials[0]).toMatchObject({ status: "error" });
    expect(b.results.trials[0]!.error).toContain("dispatch failed");
    expect(b.results.summary[0]!.errored).toBe(1);
  });

  it("records a judge that answers with an undeclared label as a score error", async () => {
    const t = await setup(plan({ configurations: [{ name: "only" }], suite: { ...suite, tasks: [suite.tasks![1]!] } }));
    await t.start();
    await t.dispatchAll();
    const tr = (await t.get()).results.trials[0]!;
    const r = t.finishTrial(tr.run_id!, "succeeded", "fixed", 1, 1);
    await t.call((i) => i.trialRunUpdate(r.id));
    const j = t.world.judges[0]!;
    finishJudge(t.world, j.runId, "succeeded", '{"label": "maybe"}', 0.1);
    await t.call((i) => i.trialJudgeUpdate(j.runId));
    const b: BenchmarkRunJson = await t.get();
    expect(b.results.trials[0]!.scores![0]!.error).toContain("undeclared label");
    expect(b.state).toBe("complete");
    expect((b.results as BenchmarkResults).summary[0]!.scored).toBe(0);
  });
});
