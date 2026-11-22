// Port of internal/scoring tests: scoring_test.go, judge_test.go,
// improve_review_test.go (judge-run paths; there is no direct model call).
import { beforeEach, describe, expect, it } from "vitest";
import type { Scorer, User } from "@openfactory/core";
import { capMiddle, truncate } from "./text";
import { eligibleReviewers, improvementSettings } from "./improve";
import { parseJudgeAnswer, resolveLabel, shouldSample } from "./sample";
import { ScoringService, deterministicRunId } from "./service";
import { buildTranscript } from "./transcript";
import { FakeWorld, finishJudge, run } from "./testkit";
import { DEFAULT_MAX_FAILURE_AGE_MS } from "./types";

const LABELS = [
  { value: "tests_run", score: 1 },
  { value: "tests_skipped", score: 0 },
];
const SHA = "0123456789abcdef0123456789abcdef01234567";

function scorer(over: Partial<Scorer> & Pick<Scorer, "id">): Scorer {
  return {
    factory_id: "fac_1",
    slug: over.id,
    name: over.id,
    agents: [{ name: "implement" }],
    labels: LABELS,
    passing_score: 1,
    sampling_rate: 100,
    model: "judge-model",
    rubric: "Did the agent run tests?",
    self_improvement: false,
    harness: { type: "claude", model: "m" },
    ...over,
  } as Scorer;
}

function setup() {
  const w = new FakeWorld();
  const svc = new ScoringService({
    store: w,
    runs: w,
    engine: w,
    benchmarks: w,
    opts: { now: w.clock.now, randomInt: () => 0, publicUrl: "https://factory.example.com/" },
  });
  w.users = [
    { id: "u_admin", email: "admin@example.com", role: "admin" },
    { id: "u_dev", email: "dev@example.com", role: "editor" },
  ];
  return { w, svc };
}

const finished = (w: FakeWorld, id: string, agent: string, state: "succeeded" | "failed", ...say: string[]) => {
  const r = w.put(run({ id, agent_name: agent, state }));
  for (const m of say) w.say(id, "assistant", m);
  return r;
};

describe("shouldSample", () => {
  const n = 5000;
  const cases: [string, number, number][] = [
    ["zero disables", 0, 0],
    ["rounds below two decimals to zero", 0.004, 0],
    ["negative", -5, 0],
    ["ten percent", 10, 0.1],
    ["quarter", 25, 0.25],
    ["all", 100, 1],
    ["over 100", 150, 1],
  ];
  it.each(cases)("%s", async (_n, rate, want) => {
    let hits = 0;
    for (let i = 0; i < n; i++) {
      const id = `run_${i}`;
      const got = await shouldSample("scorer_a", id, rate);
      if (got) hits++;
    }
    expect(Math.abs(hits / n - want)).toBeLessThan(0.03);
  });

  it("is deterministic, independent per scorer and monotonic in the rate", async () => {
    let diff = 0;
    for (let i = 0; i < 1000; i++) {
      const id = `run_${i}`;
      expect(await shouldSample("a", id, 50)).toBe(await shouldSample("a", id, 50));
      if ((await shouldSample("a", id, 50)) !== (await shouldSample("b", id, 50))) diff++;
    }
    expect(diff).toBeGreaterThan(400);
    expect(diff).toBeLessThan(600);
    for (let i = 0; i < 1000; i++) {
      const id = `run_${i}`;
      if (await shouldSample("s", id, 10)) expect(await shouldSample("s", id, 20)).toBe(true);
    }
  });

  it("matches the Go hash mapping", async () => {
    // Go: sha256("a\x00run_1") first 8 bytes >> 11 / 2^53 * 100 < rate. A rate of 50 splits runs 50/50;
    // the values below were computed with the Go implementation.
    const results: boolean[] = [];
    for (let i = 0; i < 8; i++) results.push(await shouldSample("scorer_x", `run_${i}`, 50));
    expect(results.some((x) => x)).toBe(true);
    expect(results.some((x) => !x)).toBe(true);
  });
});

describe("text helpers", () => {
  it("caps the middle and keeps character boundaries", () => {
    const s = "a".repeat(1000) + "MIDDLE" + "z".repeat(1000);
    const got = capMiddle(s, 500);
    expect(new TextEncoder().encode(got).length).toBeLessThanOrEqual(500);
    expect(got.startsWith("aaa")).toBe(true);
    expect(got.endsWith("zzz")).toBe(true);
    expect(got).not.toContain("MIDDLE");
    expect(capMiddle("short", 500)).toBe("short");
    expect(truncate("héllo wörld", 2).startsWith("h…")).toBe(true);
  });
});

describe("labels and judge answers", () => {
  it("resolves labels exactly, then ignoring case, quotes and dots", () => {
    expect(resolveLabel(LABELS, "tests_run")?.value).toBe("tests_run");
    expect(resolveLabel(LABELS, ' "Tests_Run". ')?.value).toBe("tests_run");
    expect(resolveLabel(LABELS, "`tests_skipped`")?.value).toBe("tests_skipped");
    expect(resolveLabel(LABELS, "other")).toBeUndefined();
  });

  const cases: [string, string, string][] = [
    ['{"label":"a","rationale":"r"}', "a", "r"],
    ['text {"label": "first"}\nmore {"label": "last", "rationale": "why"}', "last", "why"],
    ['```json\n{"label": "b"}\n```', "b", ""],
    ['{"x": {"label": "nested"}}', "nested", ""],
  ];
  it.each(cases)("parses %j", (input, label, rationale) => {
    expect(parseJudgeAnswer(input)).toEqual({ label, rationale });
  });
  it("finds no answer in text without a complete object", () => {
    expect(parseJudgeAnswer("no json here {")).toBeUndefined();
  });
});

describe("transcripts", () => {
  it("includes descendants only when asked", async () => {
    const { w } = setup();
    const root = finished(w, "root", "foreman", "succeeded", "dispatching", "m2", "m3", "done");
    w.put(run({ id: "child", parent_run_id: "root", agent_name: "implement" }));
    w.say("child", "tool_call", "bash: go test ./...");
    w.put(run({ id: "grand", parent_run_id: "child", agent_name: "review" }));
    w.say("grand", "assistant", "LGTM grandchild");
    const tr = await buildTranscript(w, root, true, 4000, 200_000);
    for (const want of ["=== Run root", "m3", "done", "=== Child run child", "go test", "LGTM grandchild"])
      expect(tr).toContain(want);
    const plain = await buildTranscript(w, root, false, 4000, 200_000);
    expect(plain).not.toContain("go test");
  });
});

describe("judge runs", () => {
  let w: FakeWorld;
  let svc: ScoringService;
  beforeEach(() => ({ w, svc } = setup()));

  it("scores a run with a judge run", async () => {
    w.scorers.push(
      scorer({
        id: "sc_1",
        slug: "tests-run",
        harness: { type: "claude", model: "team-model", auth: { source: "managedSecret", secretName: "TEAM_KEY" } },
        machine: "big",
        secrets: ["JUDGE_TOKEN"],
        mcp_servers: { docs: { url: "https://docs.example.com/mcp" } } as Scorer["mcp_servers"],
      }),
    );
    const long = "x".repeat(10_000) + " go test ./... passed";
    const r = finished(w, "run_a", "implement", "succeeded", long);
    w.starts.set(r.id, [{ codeForge: "GITHUB", owner: "acme", repo: "app", ref: SHA, resolved: true }]);
    await svc.handleRunUpdate(r.id);
    expect(w.judges).toHaveLength(1);
    const { req, runId } = w.judges[0]!;
    expect(req.harness).toMatchObject({ type: "claude", model: "judge-model", auth: { secretName: "TEAM_KEY" } });
    expect(req.machine).toBe("big");
    expect(req.secrets).toEqual(["JUDGE_TOKEN"]);
    expect(req.mcpServers!["docs"]).toBeDefined();
    expect(req.instructions).toContain("Did the agent run tests?");
    expect(req.instructions).toContain("`tests_run`");
    expect(req.files["transcript.md"]).toContain(long);
    expect(req.startingRepoRefs).toEqual([{ codeForge: "GITHUB", owner: "acme", repo: "app", ref: SHA }]);
    expect(req.triggeredBy).toBe("scorer:sc_1");
    expect(req.metadata).toMatchObject({ judge: "score", judge_scored_run: "run_a" });
    expect(w.scores).toHaveLength(0);

    // A repeated update does not start a second judge.
    await svc.handleRunUpdate(r.id);
    expect(w.judges).toHaveLength(1);

    finishJudge(w, runId, "succeeded", 'The tests ran.\n{"label": "tests_run", "rationale": "go test passed"}', 0.25);
    await svc.handleRunUpdate(runId);
    expect(w.scores).toHaveLength(1);
    expect(w.scores[0]).toMatchObject({
      label: "tests_run",
      passed: true,
      judge_run_id: runId,
      judge_usage: { cost_usd: 0.25 },
    });
    // The judge run is never scored, and a repeated update changes nothing.
    await svc.handleRunUpdate(runId);
    expect(w.scores).toHaveLength(1);
    expect(w.judges).toHaveLength(1);
  });

  it("stores nothing when the judge gives no label, and a new score starts a new judge", async () => {
    w.scorers.push(scorer({ id: "sc_1", slug: "s", harness: { type: "codex", model: "m" } }));
    const r = finished(w, "run_b", "implement", "failed", "oops");
    const first = await svc.scoreRun("s", r.id);
    expect(first.id).toBe("");
    const judgeId = first.judge_run_id!;
    finishJudge(w, judgeId, "succeeded", "I am not sure.", 0);
    await svc.handleRunUpdate(judgeId);
    expect(w.scores).toHaveLength(0);
    const again = await svc.scoreRun("s", r.id);
    expect(again.id).toBe("");
    expect(again.judge_run_id).not.toBe(judgeId);
    expect(w.judges).toHaveLength(2);
  });

  it("restarts an unstarted judge with the same run ID", async () => {
    w.scorers.push(scorer({ id: "sc_1", slug: "s" }));
    const r = finished(w, "run_c", "implement", "succeeded", "x");
    const id = await deterministicRunId("judge", "sc_1", r.id, "1");
    w.kv.set(`scoring:judge:sc_1:${r.id}`, JSON.stringify({ attempt: 1, run_id: id, active: true }));
    const out = await svc.scoreRun("s", r.id);
    expect(out.judge_run_id).toBe(id);
    expect(w.judges.map((j) => j.runId)).toEqual([id]);
  });

  it("stops a judge that waits for input after its answer", async () => {
    w.scorers.push(
      scorer({
        id: "sc_1",
        slug: "s",
        labels: [
          { value: "ok", score: 1 },
          { value: "bad", score: 0 },
        ],
      }),
    );
    const r = finished(w, "run_i", "implement", "succeeded", "done");
    const { judge_run_id: judgeId } = await svc.scoreRun("s", r.id);
    finishJudge(w, judgeId!, "idle", '{"label": "ok", "rationale": "fine"}', 0.1);
    await svc.handleRunUpdate(judgeId!);
    expect(w.scores).toHaveLength(1);
    expect(w.scores[0]!.label).toBe("ok");
    expect(w.cancels).toEqual([`${judgeId} by scoring`]);
  });

  it("fails for a scorer without a harness", async () => {
    w.scorers.push(scorer({ id: "sc_1", slug: "s", harness: undefined }));
    const r = finished(w, "run_d", "implement", "succeeded", "x");
    await expect(svc.scoreRun("s", r.id)).rejects.toThrow(/no harness/);
  });

  it("uses the passing score and keeps the improvement run of a replaced score", async () => {
    w.scorers.push(
      scorer({
        id: "sc_1",
        slug: "s",
        passing_score: 0.5,
        labels: [
          { value: "meh", score: 0.4 },
          { value: "ok", score: 0.6 },
        ],
      }),
    );
    const r = finished(w, "run_e", "implement", "succeeded", "x");
    w.scores.push({
      id: "score_old",
      factory_id: "fac_1",
      scorer_id: "sc_1",
      run_id: r.id,
      agent_name: "implement",
      label: "meh",
      score: 0.4,
      passed: false,
      rationale: "",
      created_at: w.clock.iso(),
      improvement_run_id: "run_imp",
    });
    const { judge_run_id } = await svc.scoreRun("s", r.id);
    finishJudge(w, judge_run_id!, "succeeded", '{"label":"ok"}', 0);
    await svc.handleRunUpdate(judge_run_id!);
    expect(w.scores).toHaveLength(1);
    expect(w.scores[0]).toMatchObject({ id: "score_old", label: "ok", passed: true, improvement_run_id: "run_imp" });
  });

  it("does not score benchmark runs and judge runs, runs in error, or runs of other agents", async () => {
    w.scorers.push(scorer({ id: "sc_1", slug: "s" }));
    const inBench = finished(w, "run_f", "implement", "succeeded", "x");
    inBench.triggered_by = "benchmark:b1";
    const child = w.put(run({ id: "run_g", agent_name: "implement", parent_run_id: inBench.id }));
    const errored = w.put(run({ id: "run_h", agent_name: "implement", state: "error" }));
    const other = finished(w, "run_j", "review", "succeeded", "x");
    for (const r of [child, errored, other]) await svc.handleRunUpdate(r.id);
    expect(w.judges).toHaveLength(0);
  });

  it("tells BenchmarkDO about a trial run and its judge", async () => {
    const trial = w.put(run({ id: "run_t", agent_name: "implement", metadata: { benchmark_run: "bench_1" } }));
    await svc.handleRunUpdate(trial.id);
    expect(w.benchRuns).toEqual([["bench_1", "run_t"]]);
    const judge = w.put(
      run({ id: "run_jt", metadata: { judge: "trial", judge_benchmark: "bench_1", judge_trial: "trial_1" } }),
    );
    await svc.handleRunUpdate(judge.id);
    expect(w.benchJudges).toEqual([["bench_1", "run_jt"]]);
  });
});

describe("self-improvement", () => {
  let w: FakeWorld;
  let svc: ScoringService;
  beforeEach(() => ({ w, svc } = setup()));

  /** Scores a run as failed through its judge. */
  const failRun = async (id: string, agent = "implement", ...say: string[]) => {
    const r = finished(w, id, agent, "succeeded", ...say);
    await svc.handleRunUpdate(r.id);
    await finishAll();
    return r;
  };
  /** Finishes every judge run that waits, with a failing label. */
  const finishAll = async () => {
    for (const j of [...w.judges]) {
      if (w.runsById.get(j.runId)!.state !== "queued") continue;
      finishJudge(w, j.runId, "succeeded", '{"label": "tests_skipped", "rationale": "no test evidence"}', 0);
      await svc.handleRunUpdate(j.runId);
    }
  };

  it("starts one run at the threshold and marks the failures", async () => {
    w.definition = {
      name: "demo",
      selfImprovement: { failedRunThreshold: 3, reviewerType: "custom", reviewerEmails: ["DEV@example.com"] },
    } as never;
    w.files = {
      "factory.yaml": "name: demo\n",
      "agents/implement/agent.md": "Always write code.",
      "agents/implement/skills/tdd/SKILL.md": "Run tests first.",
      "agents/review/agent.md": "Review things.",
      "scorers/tests-run/scorer.md": "Did the agent run tests?",
    };
    w.scorers.push(
      scorer({ id: "sc_si", slug: "tests-run", self_improvement: true }),
      scorer({ id: "sc_plain", slug: "other" }),
    );
    await failRun("run_1", "implement", "skipped tests in attempt 1");
    await failRun("run_2", "implement", "skipped tests in attempt 2");
    expect(w.dispatches).toHaveLength(0);
    const r3 = finished(w, "run_3", "implement", "failed", "skipped tests again");
    r3.error = "boom";
    await svc.handleRunUpdate(r3.id);
    await finishAll();
    expect(w.dispatches).toHaveLength(1);
    const { req, key } = w.dispatches[0]!;
    expect(req.triggeredBy).toBe("self_improvement");
    expect(req.metadata).toMatchObject({ self_improvement: "true", self_improvement_agent: "implement" });
    expect(req.source.provider).toBe("self_improvement");
    expect(key).toMatch(/^improve:fac_1:implement:[0-9a-f]{64}$/);
    for (const want of [
      "`implement`",
      "run_1",
      "run_3",
      "no test evidence",
      "Regressions addressed",
      "https://factory.example.com/runs/run_2",
      "Always write code.",
      "Run tests first.",
      "Did the agent run tests?",
      "pull request",
      "propose_definition_change",
      "ERROR: boom",
    ])
      expect(req.prompt).toContain(want);
    expect(req.prompt).not.toContain("Review things.");
    // Every failure of the self-improvement scorer is marked.
    for (const s of w.scores) {
      if (s.scorer_id === "sc_si") expect(s.improvement_run_id).toBeTruthy();
    }
    expect(w.inbox).toHaveLength(0);
    // The track records the reviewer chosen from the custom list.
    const track = JSON.parse([...w.kv.entries()].find(([k]) => k.startsWith("scoring:improve:wi:"))![1]) as {
      reviewer_id: string;
    };
    expect(track.reviewer_id).toBe("u_dev");

    // One more failure is below the threshold again.
    await failRun("run_4", "implement", "skipped");
    expect(w.dispatches).toHaveLength(1);
    // "Run now" processes an agent with one failure.
    const runs = await svc.triggerSelfImprovement("fac_1", "");
    expect(runs).toHaveLength(1);
    expect(w.dispatches).toHaveLength(2);
    expect(await svc.triggerSelfImprovement("fac_1", "implement")).toHaveLength(0);
  });

  it("counts distinct runs, triggers on age and reviews the pull request", async () => {
    w.factories.set("fac_1", {
      id: "fac_1",
      team_id: "team_1",
      name: "demo",
      source: "git",
      git_repo: "github.com/acme/def",
      active_version_id: "ver_1",
    });
    w.scorers.push(
      scorer({ id: "sc_a", slug: "a", self_improvement: true }),
      scorer({ id: "sc_b", slug: "b", self_improvement: true }),
    );
    const r = finished(w, "run_1", "implement", "succeeded", "nope");
    await svc.handleRunUpdate(r.id);
    for (const j of [...w.judges]) {
      finishJudge(w, j.runId, "succeeded", '{"label": "tests_skipped"}', 0);
      await svc.handleRunUpdate(j.runId);
    }
    expect((await svc.unreviewedFailures("fac_1")).get("implement")).toHaveLength(1);
    expect(await svc.checkSelfImprovement("fac_1")).toHaveLength(0);
    w.clock.advance(DEFAULT_MAX_FAILURE_AGE_MS);
    const started = await svc.checkSelfImprovement("fac_1");
    expect(started).toHaveLength(1);
    expect(w.inbox).toHaveLength(0);
    const imp = started[0]!;
    // The run opens a pull request. When its turn ends, the reviewer gets the item.
    w.workItems.set(imp.work_item_id!, {
      id: imp.work_item_id!,
      factory_id: "fac_1",
      pull_requests: [
        {
          forge: "GITHUB",
          repo: "acme/def",
          number: 7,
          url: "https://github.com/acme/def/pull/7",
          branch: "",
          state: "open",
        },
      ],
    } as never);
    imp.state = "idle";
    await svc.handleRunUpdate(imp.id);
    expect(w.inbox).toHaveLength(1);
    expect(w.inbox[0]).toMatchObject({
      assignee_id: "u_admin",
      kind: "review",
      run_id: imp.id,
      work_item_id: imp.work_item_id,
    });
    expect(w.inbox[0]!.body).toContain("acme/def#7");
    expect(w.inbox[0]!.body).toContain("Regressions addressed");
    // The same pull request does not create a second item.
    await svc.handleRunUpdate(imp.id);
    expect(w.inbox).toHaveLength(1);
  });

  it("asks the reviewer on the forge, and notes a refused request", async () => {
    const asked: string[] = [];
    let fail = "";
    svc = new ScoringService({
      store: w,
      runs: w,
      engine: w,
      benchmarks: w,
      opts: {
        now: w.clock.now,
        randomInt: () => 0,
        requestReview: async (_t, pr, user) => {
          asked.push(`${user}@${pr.repo}`);
          if (fail) throw new Error(fail);
        },
      },
    });
    w.users = [{ id: "u_admin", email: "a@x.com", role: "admin" }];
    w.scorers.push(scorer({ id: "sc_a", slug: "a", self_improvement: true }));
    const r = finished(w, "run_1", "implement", "succeeded", "nope");
    await svc.handleRunUpdate(r.id);
    const j = w.judges[0]!;
    finishJudge(w, j.runId, "succeeded", '{"label": "tests_skipped"}', 0);
    await svc.handleRunUpdate(j.runId);
    const [imp] = await svc.triggerSelfImprovement("fac_1", "");
    imp!.state = "idle";
    await svc.handleRunUpdate(imp!.id);
    expect(w.inbox).toHaveLength(0);
    const wi = w.workItems.get(imp!.work_item_id!)!;
    wi.pull_requests = [
      { forge: "GITHUB", repo: "acme/def", number: 6, url: "u6", branch: "", state: "merged" },
      { forge: "GITHUB", repo: "acme/def", number: 7, url: "u7", branch: "", state: "open" },
    ];
    await svc.handleRunUpdate(imp!.id);
    expect(asked).toEqual(["u_admin@acme/def"]);
    expect(w.inbox[0]!.body).toContain("A review is requested from the reviewer");
    fail = "user has no linked account";
    wi.pull_requests.push({ forge: "GITHUB", repo: "acme/app", number: 3, url: "u3", branch: "", state: "draft" });
    await svc.handleRunUpdate(imp!.id);
    expect(w.inbox).toHaveLength(2);
    expect(w.inbox[1]!.body).toContain("could not be requested");
    expect(w.inbox[1]!.body).toContain("no linked account");
  });
});

describe("improvement settings and reviewers", () => {
  const cases: [Parameters<typeof improvementSettings>[0], number][] = [
    [null, 25],
    [{}, 25],
    [{ selfImprovement: {} }, 25],
    [{ selfImprovement: { failedRunThreshold: 7 } }, 7],
    [{ selfImprovement: { failedRunThreshold: 99 } }, 50],
  ];
  it.each(cases)("threshold of %j is %i", (def, want) => {
    expect(improvementSettings(def, {}).threshold).toBe(want);
  });
  it("defaults the reviewer type and takes the analysis model from the definition, then the server", () => {
    expect(improvementSettings(null, {}).reviewerType).toBe("admins");
    expect(improvementSettings(null, { analysisModel: "server-model" }).analysisModel).toBe("server-model");
    expect(
      improvementSettings({ selfImprovement: { analysisModel: "def-model" } }, { analysisModel: "server-model" })
        .analysisModel,
    ).toBe("def-model");
  });

  const users = [
    { id: "b", email: "B@x.com", role: "admin" },
    { id: "a", email: "a@x.com", role: "editor" },
    { id: "c", email: "c@x.com", role: "user" },
  ] as Pick<User, "id" | "email" | "role">[];
  const reviewer: [string, string[], string][] = [
    ["admins", [], "b"],
    ["", [], "b"],
    ["team", [], "a,b,c"],
    ["custom", ["b@x.com", " c@X.com "], "b,c"],
    ["custom", ["nobody@x.com"], ""],
    ["none", [], ""],
  ];
  it.each(reviewer)("%s %j gives %j", (type, emails, want) => {
    expect(
      eligibleReviewers(users, type, emails)
        .map((u) => u.id)
        .join(","),
    ).toBe(want);
  });
});
