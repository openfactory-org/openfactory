// Pure benchmark and proposal tests: port of TestAggregate, TestRepoSubstitutions,
// TestCorrectnessScorerRubric, TestBenchmarkValidation (request part), TestDiffFiles,
// TestMakeTaskFromRun.
import { describe, expect, it } from "vitest";
import {
  CORRECTNESS_SCORER, aggregate, applyRepoSubstitutions, benchmarkState, correctnessScorerFor, formatRepoAtCommit, validateConfigurations,
  type Trial,
} from "./benchmark";
import { addSuiteTask, makeTaskFromRun } from "./benchmark-tasks";
import { cleanDefinitionPath, diffFiles, mergeProposal } from "./proposals";
import { FakeWorld, run } from "./testkit";

const SHA = "0123456789abcdef0123456789abcdef01234567";
const fin = new Date().toISOString();

function trial(cfg: string, correct: boolean, cost: number, dur: number, scored: boolean): Trial {
  const t: Trial = { id: "t", configuration: cfg, task: "x", repetition: 1, status: "finished", run_id: "r", finished_at: fin, cost_usd: cost, input_tokens: 0, output_tokens: 0, duration_ms: dur, scored };
  if (scored) t.scores = [{ scorer: CORRECTNESS_SCORER, score: correct ? 1 : 0, passed: correct }];
  return t;
}
const blank = (o: Partial<Trial>): Trial => ({ id: "t", configuration: "", task: "x", repetition: 1, status: "pending", cost_usd: 0, input_tokens: 0, output_tokens: 0, duration_ms: 0, scored: false, ...o });

describe("aggregate", () => {
  it("summarizes configurations and recommends", () => {
    const configs = [{ name: "A" }, { name: "B" }, { name: "C" }];
    const trials: Trial[] = [
      trial("A", true, 1, 100, true), trial("A", false, 3, 300, true), trial("A", true, 2, 200, true),
      trial("B", true, 0.5, 50, true), trial("B", true, 0.5, 70, true), trial("B", false, 0.5, 90, false),
      blank({ configuration: "B", status: "running", run_id: "r" }),
      blank({ configuration: "C", status: "error", error: "boom" }),
      { ...trial("B", false, 0, 0, true), scores: [{ scorer: CORRECTNESS_SCORER, score: 0, passed: false, error: "judge down" }] },
    ];
    const { summary, recommendations, total } = aggregate(configs, trials, [CORRECTNESS_SCORER]);
    const [a, b, c] = summary;
    expect(a).toMatchObject({ scored: 3, passed: 2, median_duration_ms: 200, avg_duration_ms: 200 });
    expect(a!.pass_rate).toBeCloseTo(2 / 3);
    expect(a!.avg_cost_usd).toBeCloseTo(2);
    expect(b).toMatchObject({ trials: 5, finished: 4, scored: 2, pass_rate: 1, median_duration_ms: 60 });
    expect(b!.total_cost_usd).toBeCloseTo(1.5);
    expect(c).toMatchObject({ trials: 1, errored: 1, finished: 0, scorer_averages: {} });
    expect(recommendations).toEqual({ overall: "B", lowest_cost: "B", most_efficient: "B" });
    expect(total).toBeCloseTo(7.5);
    expect(aggregate(configs.slice(0, 1), trials.slice(0, 3), [CORRECTNESS_SCORER]).recommendations).toEqual({});
    expect(aggregate([], [trial("X", true, 2, 1, true), trial("Y", true, 1, 1, true)], [CORRECTNESS_SCORER]).recommendations.overall).toBe("Y");
    const eq = aggregate([], [trial("X", true, 1, 1, true), trial("Y", false, 1, 1, true)], [CORRECTNESS_SCORER]).recommendations;
    expect(eq).toEqual({ overall: "X" });
  });
  it("derives the run state", () => {
    expect(benchmarkState([blank({ status: "running" })])).toBe("running");
    expect(benchmarkState([trial("A", true, 1, 1, false)])).toBe("scoring");
    expect(benchmarkState([trial("A", true, 1, 1, true), blank({ status: "error" })])).toBe("complete");
  });
});

describe("repo substitutions and rubric", () => {
  it("replaces matching repositories and keeps the input", () => {
    const refs = [
      { codeForge: "GITHUB", owner: "acme", repo: "app", ref: SHA },
      { codeForge: "GITLAB", owner: "acme", repo: "app", ref: SHA },
    ];
    const subs = [{ source: { codeForge: "GITHUB", owner: "Acme", name: "App" }, target: { codeForge: "GITHUB", owner: "bench", name: "app" } }];
    const got = applyRepoSubstitutions(refs, subs);
    expect(got[0]).toEqual({ codeForge: "GITHUB", owner: "bench", repo: "app", ref: SHA });
    expect(got[1]).toEqual(refs[1]);
    expect(refs[0]!.owner).toBe("acme");
    expect(formatRepoAtCommit(refs[1]!)).toBe(`gitlab.com:acme/app@${SHA}`);
  });
  it("builds the correctness rubric", () => {
    const sc = correctnessScorerFor({ title: "T", prompt: "Do X", successCriteria: "X is done" }, "m");
    expect(sc).toMatchObject({ key: "correctness", model: "m", passing_score: 1 });
    expect(sc.labels).toHaveLength(2);
    expect(sc.rubric).toContain("X is done");
    expect(sc.rubric).toContain("Do X");
  });
});

describe("validateConfigurations", () => {
  const many = Array.from({ length: 7 }, (_, i) => ({ name: String.fromCharCode(97 + i) }));
  const agents = [{ name: "implement" }];
  const bad: [string, Parameters<typeof validateConfigurations>[0]][] = [
    ["too many", many],
    ["duplicate", [{ name: "a" }, { name: "a" }]],
    ["model and harness", [{ name: "a", agents: [{ agent: "implement", model: "m", harness: { type: "codex", model: "m" } }] }]],
    ["unknown agent", [{ name: "a", agents: [{ agent: "ghost", model: "m" }] }]],
    ["unknown machine", [{ name: "a", machine: "huge" }]],
  ];
  it.each(bad)("rejects %s", (_n, cfgs) => {
    expect(() => validateConfigurations(cfgs, agents, {})).toThrow();
  });
  it("accepts a valid pin", () => {
    expect(() => validateConfigurations([{ name: "a", agents: [{ agent: "implement", model: "m" }] }], agents, {})).not.toThrow();
  });
});

describe("proposals", () => {
  it("diffs trees", () => {
    const d = diffFiles({ a: "1\n2\n3\n", gone: "x\n" }, { a: "1\n3\n4\n", new: "y\n" });
    expect(d).toContain("--- a/a\n+++ b/a\n@@ -1,3 +1,3 @@\n 1\n-2\n 3\n+4\n");
    expect(d).toContain("--- a/gone\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-x\n");
    expect(d).toContain("--- /dev/null\n+++ b/new\n@@ -0,0 +1,1 @@\n+y\n");
    expect(diffFiles({ a: "1" }, { a: "1" })).toBe("");
    const long = Array.from({ length: 100 }, (_, i) => `line ${i + 1}`);
    const old = long.join("\n") + "\n";
    long[49] = "changed";
    const d2 = diffFiles({ f: old }, { f: long.join("\n") + "\n" });
    expect(d2).toContain("@@ -47,7 +47,7 @@\n line 47\n line 48\n line 49\n-line 50\n+changed\n line 51\n");
    expect(d2).not.toContain("line 1\n");
    expect(d2.split("\n").length - 1).toBeLessThanOrEqual(12);
  });
  it("cleans paths and merges onto the root of the active tree", () => {
    expect(cleanDefinitionPath("agents/a/agent.md")).toBe("agents/a/agent.md");
    for (const p of ["../x", "/abs", "a/../b", ""]) expect(() => cleanDefinitionPath(p)).toThrow();
    const base = { "defs/factory.yaml": "name: x\n", "defs/old.md": "o" };
    const out = mergeProposal(base, { files: { "agents/a.md": "A" }, delete: ["old.md"] });
    expect(Object.keys(out.files).sort()).toEqual(["defs/agents/a.md", "defs/factory.yaml"]);
    expect(out.diff).toContain("+A");
    expect(() => mergeProposal(base, { files: { "factory.yaml": "name: x\n" } })).toThrow(/does not change/);
    expect(() => mergeProposal(base, { files: {}, delete: ["ghost"] })).toThrow(/does not exist/);
    expect(() => mergeProposal(base, { files: {} })).toThrow();
  });
});

describe("makeTaskFromRun", () => {
  const files = { "factory.yaml": "name: x\n", "benchmarks/fix/suite.yaml": "name: Fix\nagent: implement\ntasks:\n  - broken-link\n" };
  const active = { definition: { benchmarks: [{ slug: "fix", name: "Fix", agent: "implement" }] }, files };
  it("builds the task, its file and the suite change", async () => {
    const w = new FakeWorld();
    w.put(run({ id: "run_src", agent_name: "implement", prompt: "Fix the login page.", name: "Login fix" }));
    const req = { factoryId: "fac_1", suite: "fix", runId: "run_src", successCriteria: "Login works." };
    expect((await makeTaskFromRun(w, active, req)).task.startingRepoRefs).toBeUndefined();
    w.starts.set("run_src", [{ codeForge: "GITHUB", owner: "acme", repo: "app", ref: "main" }]);
    await expect(makeTaskFromRun(w, active, req)).rejects.toThrow(/start commit/);
    w.starts.set("run_src", [{ codeForge: "GITHUB", owner: "acme", repo: "app", ref: SHA, resolved: true }]);
    const got = await makeTaskFromRun(w, active, req);
    expect(got.task.slug).toBe("login-fix-run-src");
    expect(got.path).toBe("benchmarks/fix/tasks/login-fix-run-src.yaml");
    expect(got.files[got.path]).toContain(`github.com:acme/app@${SHA}`);
    expect(got.files[got.path]).toContain("sourceRunId: run_src");
    expect(got.files["benchmarks/fix/suite.yaml"]).toContain("- broken-link");
    expect(got.files["benchmarks/fix/suite.yaml"]).toContain("- login-fix-run-src");
    expect(got.definition["factory.yaml"]).toBeTruthy();
    await expect(makeTaskFromRun(w, active, { ...req, successCriteria: "" })).rejects.toThrow(/success_criteria/);
  });
  it("adds the tasks list when the suite has none", () => {
    expect(addSuiteTask("name: Fix\n", "t1")).toContain("- t1");
  });
});
