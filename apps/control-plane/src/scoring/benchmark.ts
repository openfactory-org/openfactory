// Benchmarks: types, trial lifecycle rules, aggregation and trial requests.
// Port of the pure parts of benchmark.go. BenchmarkDO (src/do/benchmark) runs them.
import { stringify as yamlStringify } from "yaml";
import {
  OfError,
  type BenchmarkAgentPin,
  type BenchmarkConfiguration,
  type BenchmarkSuite,
  type BenchmarkTask,
  type MachineSpec,
  type RepoAtCommit,
  type RepoSubstitution,
  type RunState,
  type ScorerLabel,
} from "@openfactory/core";
import { clip } from "./text";
import {
  MAX_BENCHMARK_CONFIGURATIONS,
  MAX_METADATA_VALUE,
  META_BENCHMARK_CONFIGURATION,
  META_BENCHMARK_REPETITION,
  META_BENCHMARK_RUN,
  META_BENCHMARK_TASK,
  META_BENCHMARK_TRIAL,
  META_TRIAL_MODE,
  type DispatchRequest,
  type JudgeExec,
  type Overrides,
} from "./types";

// Benchmark run states.
export const BENCHMARK_RUNNING = "running";
export const BENCHMARK_SCORING = "scoring";
export const BENCHMARK_COMPLETE = "complete";
export const BENCHMARK_FAILED = "failed";
export const BENCHMARK_CANCELLED = "cancelled";

/** The key of the built-in Correctness Scorer in trial results. */
export const CORRECTNESS_SCORER = "correctness";

export type TrialStatus = "pending" | "running" | "finished" | "blocked" | "error" | "cancelled";

export interface TrialScore {
  scorer: string;
  label?: string;
  score: number;
  passed: boolean;
  rationale?: string;
  error?: string;
  judge_run_id?: string;
  pending?: boolean;
  judge_cost_usd?: number;
}

export interface Trial {
  id: string;
  configuration: string;
  task: string;
  repetition: number;
  status: TrialStatus;
  run_id?: string;
  work_item_id?: string;
  run_state?: RunState;
  error?: string;
  starting_repo_refs?: RepoAtCommit[];
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  duration_ms: number;
  started_at?: string;
  finished_at?: string;
  scored: boolean;
  scores?: TrialScore[];
  judge_cost_usd?: number;
}

/** A trial needs no further work. */
export function trialDone(t: Pick<Trial, "status" | "scored" | "run_id">): boolean {
  switch (t.status) {
    case "error":
      return true;
    case "cancelled":
      return t.scored || !t.run_id;
    case "finished":
    case "blocked":
      return t.scored;
  }
  return false;
}

export const trialRunFinished = (t: Pick<Trial, "status">): boolean =>
  t.status === "finished" || t.status === "blocked" || t.status === "cancelled" || t.status === "error";

export const trialScore = (t: Pick<Trial, "scores">, key: string): TrialScore | undefined => t.scores?.find((s) => s.scorer === key);

/** A scorer used by a benchmark run, copied so that later edits do not change past results. */
export interface BenchmarkScorer extends JudgeExec {
  key: string;
  scorer_id?: string;
  name: string;
  model: string;
  rubric: string;
  labels: ScorerLabel[];
  passing_score: number;
}

export interface ConfigurationResult {
  name: string;
  trials: number;
  finished: number;
  blocked: number;
  errored: number;
  scored: number;
  passed: number;
  pass_rate: number;
  scorer_averages: Record<string, number>;
  overall: number;
  total_cost_usd: number;
  avg_cost_usd: number;
  avg_duration_ms: number;
  median_duration_ms: number;
  input_tokens: number;
  output_tokens: number;
  judge_cost_usd: number;
}

export interface Recommendations {
  overall?: string;
  most_efficient?: string;
  lowest_cost?: string;
}

/** The JSON in BenchmarkRun.results. It keeps the run's inputs. */
export interface BenchmarkResults {
  suite: BenchmarkSuite;
  configurations: BenchmarkConfiguration[];
  repetitions: number;
  correctness_model: string;
  correctness_judge: JudgeExec;
  scorers?: BenchmarkScorer[];
  trials: Trial[];
  summary: ConfigurationResult[];
  recommendations: Recommendations;
  total_cost_usd: number;
  judge_cost_usd: number;
  cancelled_by?: string;
}

export const benchmarkTerminal = (state: string): boolean =>
  state === BENCHMARK_COMPLETE || state === BENCHMARK_FAILED || state === BENCHMARK_CANCELLED;

/** The state of a run from its trials. */
export function benchmarkState(trials: readonly Trial[]): string {
  const allRunsDone = trials.every(trialRunFinished);
  const allDone = trials.every(trialDone);
  if (allDone) return BENCHMARK_COMPLETE;
  if (allRunsDone) return BENCHMARK_SCORING;
  return BENCHMARK_RUNNING;
}

// ---------- Aggregation ----------

function medianInt(xs: readonly number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  const n = s.length;
  if (n === 0) return 0;
  if (n % 2 === 1) return s[(n - 1) / 2]!;
  return Math.trunc((s[n / 2 - 1]! + s[n / 2]!) / 2);
}

/**
 * Per-configuration results, recommendations and the total cost of the trials
 * and their judge runs. A pure function of the trials. The pass rate counts
 * Correctness Scorer results; judge errors are excluded from counts and averages.
 */
export function aggregate(
  configs: readonly BenchmarkConfiguration[],
  trials: readonly Trial[],
  scorerKeys: readonly string[],
): { summary: ConfigurationResult[]; recommendations: Recommendations; total: number } {
  const order: string[] = [];
  const byName = new Map<string, ConfigurationResult>();
  const add = (name: string): ConfigurationResult => {
    let r = byName.get(name);
    if (!r) {
      r = {
        name, trials: 0, finished: 0, blocked: 0, errored: 0, scored: 0, passed: 0, pass_rate: 0, scorer_averages: {}, overall: 0,
        total_cost_usd: 0, avg_cost_usd: 0, avg_duration_ms: 0, median_duration_ms: 0, input_tokens: 0, output_tokens: 0, judge_cost_usd: 0,
      };
      byName.set(name, r);
      order.push(name);
    }
    return r;
  };
  for (const c of configs) add(c.name);
  const sums = new Map<string, Map<string, { sum: number; n: number }>>();
  const durations = new Map<string, number[]>();
  let total = 0;
  for (const t of trials) {
    const r = add(t.configuration);
    r.trials++;
    r.judge_cost_usd += t.judge_cost_usd ?? 0;
    total += t.judge_cost_usd ?? 0;
    if (t.status === "error") r.errored++;
    else if (t.status === "blocked") r.blocked++;
    if (t.run_id && t.finished_at) {
      r.finished++;
      r.total_cost_usd += t.cost_usd;
      r.input_tokens += t.input_tokens;
      r.output_tokens += t.output_tokens;
      durations.set(t.configuration, [...(durations.get(t.configuration) ?? []), t.duration_ms]);
      total += t.cost_usd;
    }
    if (!t.scored) continue;
    const cs = trialScore(t, CORRECTNESS_SCORER);
    if (cs && !cs.error) {
      r.scored++;
      if (cs.passed) r.passed++;
    }
    for (const key of scorerKeys) {
      const ts = trialScore(t, key);
      if (!ts || ts.error) continue;
      let m = sums.get(t.configuration);
      if (!m) sums.set(t.configuration, (m = new Map()));
      let a = m.get(key);
      if (!a) m.set(key, (a = { sum: 0, n: 0 }));
      a.sum += ts.score;
      a.n++;
    }
  }
  const summary: ConfigurationResult[] = [];
  for (const name of order) {
    const r = byName.get(name)!;
    if (r.scored > 0) r.pass_rate = r.passed / r.scored;
    let overall = 0;
    let n = 0;
    for (const key of scorerKeys) {
      const a = sums.get(name)?.get(key);
      if (a && a.n > 0) {
        const avg = a.sum / a.n;
        r.scorer_averages[key] = avg;
        overall += avg;
        n++;
      }
    }
    if (n > 0) r.overall = overall / n;
    if (r.finished > 0) r.avg_cost_usd = r.total_cost_usd / r.finished;
    const d = durations.get(name) ?? [];
    if (d.length > 0) {
      r.avg_duration_ms = Math.trunc(d.reduce((x, y) => x + y, 0) / d.length);
      r.median_duration_ms = medianInt(d);
    }
    summary.push(r);
  }
  return { summary, recommendations: recommend(summary), total };
}

function recommend(results: readonly ConfigurationResult[]): Recommendations {
  const cands = results.filter((r) => Object.keys(r.scorer_averages).length > 0);
  const rec: Recommendations = {};
  if (cands.length < 2) return rec;
  const eps = 1e-9;
  let best = cands[0]!;
  for (const c of cands.slice(1)) {
    if (c.overall > best.overall + eps || (Math.abs(c.overall - best.overall) <= eps && c.avg_cost_usd < best.avg_cost_usd - eps)) best = c;
  }
  rec.overall = best.name;
  let cheapest = cands[0]!;
  let allSameCost = true;
  for (const c of cands.slice(1)) {
    if (Math.abs(c.avg_cost_usd - cands[0]!.avg_cost_usd) > eps) allSameCost = false;
    if (c.avg_cost_usd < cheapest.avg_cost_usd - eps) cheapest = c;
  }
  if (!allSameCost) {
    rec.lowest_cost = cheapest.name;
    let bestEff = 0;
    let found = false;
    for (const c of cands) {
      if (c.avg_cost_usd <= 0 || c.overall <= 0) continue;
      const eff = c.overall / c.avg_cost_usd;
      if (!found || eff > bestEff + eps) {
        bestEff = eff;
        found = true;
        rec.most_efficient = c.name;
      }
    }
  }
  return rec;
}

// ---------- Request validation ----------

export function validateConfigurations(
  configs: readonly BenchmarkConfiguration[],
  agents: readonly { name: string }[] | null | undefined,
  machines: Record<string, MachineSpec> | null | undefined,
): void {
  const bad = (msg: string): never => {
    throw new OfError("invalid_request", msg);
  };
  if (configs.length > MAX_BENCHMARK_CONFIGURATIONS) bad(`at most ${MAX_BENCHMARK_CONFIGURATIONS} configurations`);
  const known = new Set((agents ?? []).map((a) => a.name));
  const names = new Set<string>();
  for (const c of configs) {
    if (c.name.trim() === "") bad("configuration name is required");
    if (names.has(c.name)) bad(`duplicate configuration name ${JSON.stringify(c.name)}`);
    names.add(c.name);
    if (c.machine && !(machines && c.machine in machines)) bad(`configuration ${JSON.stringify(c.name)}: unknown machine ${JSON.stringify(c.machine)}`);
    const pinned = new Set<string>();
    for (const p of c.agents ?? []) {
      if (!p.agent) bad(`configuration ${JSON.stringify(c.name)}: agent is required`);
      if (known.size > 0 && !known.has(p.agent)) bad(`configuration ${JSON.stringify(c.name)}: unknown agent ${JSON.stringify(p.agent)}`);
      if (pinned.has(p.agent)) bad(`configuration ${JSON.stringify(c.name)} lists agent ${JSON.stringify(p.agent)} more than once`);
      pinned.add(p.agent);
      const hasModel = !!p.model;
      const hasHarness = !!p.harness;
      if (hasModel === hasHarness) bad(`configuration ${JSON.stringify(c.name)} agent ${JSON.stringify(p.agent)}: set exactly one of model or harness`);
      if (hasHarness && (!p.harness!.type || !p.harness!.model)) {
        bad(`configuration ${JSON.stringify(c.name)} agent ${JSON.stringify(p.agent)}: harness requires type and model`);
      }
    }
  }
}

export function findSuite(suites: readonly BenchmarkSuite[] | null | undefined, slug: string): BenchmarkSuite | undefined {
  const list = suites ?? [];
  return list.find((s) => s.slug === slug) ?? list.find((s) => s.name === slug);
}

// ---------- Trial requests ----------

const normalizeHarness = (t: string): string => (t.toLowerCase() === "claude-code" ? "claude" : t.toLowerCase());

function pinOverrides(p: BenchmarkAgentPin): Overrides | undefined {
  if (p.harness) return { harness: { ...p.harness, type: normalizeHarness(p.harness.type) } };
  if (p.model) return { model: p.model };
  return undefined;
}

/** "github.com:OWNER/REPO@SHA". */
export function formatRepoAtCommit(r: RepoAtCommit): string {
  let host = r.codeForge.toLowerCase();
  switch (r.codeForge.toUpperCase()) {
    case "GITHUB":
      host = "github.com";
      break;
    case "GITLAB":
      host = "gitlab.com";
      break;
  }
  return `${host}:${r.owner}/${r.repo}@${r.ref}`;
}

export function trialPrompt(suite: BenchmarkSuite, cfg: BenchmarkConfiguration, task: BenchmarkTask, t: Pick<Trial, "repetition" | "starting_repo_refs">, reps: number): string {
  let b = task.prompt.trim();
  b += "\n\n---\n";
  b += `Benchmark trial: suite ${JSON.stringify(suite.name)}, configuration ${JSON.stringify(cfg.name)}, task ${JSON.stringify(task.slug)}, repetition ${t.repetition} of ${reps}.\n`;
  if (t.starting_repo_refs && t.starting_repo_refs.length > 0) {
    b += "Starting state: check out exactly these commits before you begin, and work from them:\n";
    for (const r of t.starting_repo_refs) b += `- ${formatRepoAtCommit(r)}\n`;
  }
  b +=
    "This trial runs in trial mode: the write tools of the integrations (tracker comments, chat messages) and your updates are recorded in this run. They do not reach the real services.\n";
  if (task.trackerSeed !== undefined && task.trackerSeed !== null) {
    b += "Tracker start state: the tracker of this trial starts with this state. Treat it as the current state of the tracker:\n\n```yaml\n";
    b += yamlStringify(task.trackerSeed);
    b += "```\n";
  }
  return b;
}

/** The engine request for one trial. The trial runs in trial mode: child runs inherit the metadata. */
export function trialDispatch(
  bench: { id: string; factoryId: string },
  suite: BenchmarkSuite,
  cfg: BenchmarkConfiguration,
  task: BenchmarkTask,
  t: Pick<Trial, "id" | "repetition" | "starting_repo_refs">,
  reps: number,
  agents: readonly string[],
): DispatchRequest {
  const md: Record<string, string> = {
    [META_TRIAL_MODE]: "true",
    [META_BENCHMARK_RUN]: bench.id,
    [META_BENCHMARK_TRIAL]: t.id,
    [META_BENCHMARK_CONFIGURATION]: clip(cfg.name, MAX_METADATA_VALUE),
    [META_BENCHMARK_TASK]: clip(task.slug, MAX_METADATA_VALUE),
    [META_BENCHMARK_REPETITION]: String(t.repetition),
  };
  let overrides: Overrides | undefined;
  let pins: Record<string, Overrides> | undefined;
  for (const p of cfg.agents ?? []) {
    const ov = pinOverrides(p);
    if (p.agent === suite.agent) {
      overrides = ov;
      continue;
    }
    if (!ov) continue;
    (pins ??= {})[p.agent] = ov;
  }
  if (cfg.machine) {
    // The machine of the configuration applies to every agent of the trial.
    overrides = { ...(overrides ?? {}), machine: cfg.machine };
    for (const a of agents) {
      if (a === suite.agent) continue;
      pins ??= {};
      pins[a] = { ...(pins[a] ?? {}), machine: cfg.machine };
    }
  }
  const req: DispatchRequest = {
    factoryId: bench.factoryId,
    prompt: trialPrompt(suite, cfg, task, t, reps),
    source: { provider: "benchmark", kind: "task", external_id: `${bench.id}/${t.id}`, title: task.title },
    agentName: suite.agent,
    title: `Benchmark ${suite.name} · ${cfg.name} · ${task.title} #${t.repetition}`,
    metadata: md,
    triggeredBy: "benchmark:" + bench.id,
  };
  if (overrides) req.overrides = overrides;
  if (t.starting_repo_refs && t.starting_repo_refs.length > 0) req.startingRepoRefs = t.starting_repo_refs;
  if (pins) req.agentPins = pins;
  return req;
}

/** Replaces each repository that matches a substitution source (ignoring case) with its target. Commits stay. */
export function applyRepoSubstitutions(refs: readonly RepoAtCommit[] | undefined, subs: readonly RepoSubstitution[] | undefined): RepoAtCommit[] {
  if (!refs || refs.length === 0) return [];
  const out = refs.map((r) => ({ ...r }));
  for (const r of out) {
    for (const sub of subs ?? []) {
      if (
        r.codeForge.toLowerCase() === sub.source.codeForge.toLowerCase() &&
        r.owner.toLowerCase() === sub.source.owner.toLowerCase() &&
        r.repo.toLowerCase() === sub.source.name.toLowerCase()
      ) {
        if (sub.target.codeForge) r.codeForge = sub.target.codeForge.toUpperCase();
        r.owner = sub.target.owner;
        r.repo = sub.target.name;
        break;
      }
    }
  }
  return out;
}

/** The built-in Correctness Scorer of a task: it judges a trial against the task's successCriteria. */
export function correctnessScorerFor(task: Pick<BenchmarkTask, "title" | "prompt" | "successCriteria">, model: string): BenchmarkScorer {
  let r = "You are the Correctness Scorer for a benchmark trial. Decide whether the agent's work satisfies the task's correctness criteria.\n\n";
  r += `Task: ${task.title}\n\nTask prompt:\n${task.prompt.trim()}\n\n`;
  r += `Correctness criteria (what a successful trial must do):\n${task.successCriteria.trim()}\n\n`;
  r +=
    "Judge only from the evidence in the transcript, including child runs. A trial that stopped early or is blocked waiting for a human may still satisfy the criteria if its evidence shows it. Return exactly one declared label.";
  return {
    key: CORRECTNESS_SCORER,
    name: "Correctness",
    model,
    rubric: r,
    labels: [
      { value: "correct", description: "The trial satisfies every correctness criterion.", score: 1 },
      { value: "incorrect", description: "The trial does not satisfy one or more correctness criteria.", score: 0 },
    ],
    passing_score: 1,
  };
}

/** The scorers of a trial: the Correctness Scorer of its task, then the extra scorers of the benchmark. */
export function trialScorers(res: Pick<BenchmarkResults, "suite" | "correctness_model" | "correctness_judge" | "scorers">, t: Pick<Trial, "task">): BenchmarkScorer[] {
  const task = res.suite.tasks?.find((tk) => tk.slug === t.task);
  const cs = correctnessScorerFor(task ?? { title: "", prompt: "", successCriteria: "" }, res.correctness_model);
  Object.assign(cs, res.correctness_judge);
  return [cs, ...(res.scorers ?? [])];
}
