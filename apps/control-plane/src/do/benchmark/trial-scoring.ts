// The scoring part of a benchmark run: it finalizes a trial when its root run
// finishes, starts one judge run per scorer, records the judge answers, and
// re-checks unfinished trials (reconcile). BenchmarkDO calls it under its
// per-DO lock; network calls run outside any transaction and each state
// change is one `persist` (one transaction).
import { OfError, clipBytes as clip, toOfError, type Run } from "@openfactory/core";
import {
  benchmarkTerminal,
  trialDone,
  trialRunFinished,
  trialScore,
  trialScorers,
  type BenchmarkScorer,
  type Trial,
  type TrialScore,
} from "../../scoring/benchmark";
import { deterministicRunId, judgeAnswer, judgeRequest, treeUsage } from "../../scoring/judge";
import { resolveLabel } from "../../scoring/sample";
import {
  JUDGE_TRIAL,
  MAX_METADATA_VALUE,
  META_JUDGE,
  META_JUDGE_BENCHMARK,
  META_JUDGE_KEY,
  META_JUDGE_SCORED_RUN,
  META_JUDGE_TRIAL,
  finishedForScoring,
  isTerminalState,
  waitsForInput,
  type EnginePort,
  type JudgeExec,
  type RunsPort,
} from "../../scoring/types";
import { iso, parsePlan, type BenchmarkPlan, type BenchmarkRepo } from "./repo";

export interface BenchmarkDeps {
  runs: RunsPort;
  engine: EnginePort;
}

/** What the coordinator needs from BenchmarkDO. */
export interface TrialScoringHost {
  readonly repo: BenchmarkRepo;
  port(): BenchmarkDeps;
  /** The clock of the timestamps in the trials. */
  clock(): number;
  /** Writes the changed trials and the state of the run in one transaction and projects the run. */
  persist(changed: readonly Trial[]): unknown;
}

export class TrialScoringCoordinator {
  constructor(private readonly host: TrialScoringHost) {}

  private get repo(): BenchmarkRepo {
    return this.host.repo;
  }

  /** A root run of a trial finished or waits for input. */
  async onTrialRun(runId: string): Promise<void> {
    const row = this.repo.row();
    if (!row || benchmarkTerminal(row.state)) return;
    const { runs } = this.host.port();
    const run = await runs.getRun(runId);
    if (!run || !finishedForScoring(run.state)) return;
    // Descendants of a trial inherit the metadata; only the root run is a trial.
    const trialId = run.metadata?.["benchmark_trial"];
    if (!trialId || run.parent_run_id) return;
    const t = this.repo.trial(trialId);
    if (!t) return;
    if (!t.run_id) {
      // The run finished before the dispatch step recorded it: adopt it.
      t.run_id = run.id;
      if (run.work_item_id) t.work_item_id = run.work_item_id;
      t.status = "running";
      t.started_at = run.created_at;
    } else if (t.run_id !== run.id) {
      return;
    }
    if (!trialRunFinished(t)) {
      await this.finalizeTrial(row.factory_id, t, run);
      this.host.persist([t]);
    } else if (t.scored) {
      return;
    }
    await this.scoreTrial(trialId);
  }

  /** Fills the outcome, the usage (with descendants) and the duration of a trial. */
  private async finalizeTrial(factoryId: string, t: Trial, run: Run): Promise<void> {
    const { runs } = this.host.port();
    t.run_state = run.state;
    switch (run.state) {
      case "cancelled":
        t.status = "cancelled";
        break;
      case "error":
        t.status = "error"; // no scoring: the run did not produce evidence to judge
        break;
      case "idle":
      case "blocked": {
        t.status = "finished";
        const items = await runs.listOpenInbox(factoryId);
        if (items.some((it) => it.run_id === run.id || (t.work_item_id && it.work_item_id === t.work_item_id)))
          t.status = "blocked";
        break;
      }
      default:
        t.status = "finished";
    }
    if (run.error) t.error = run.error;
    const usage = await treeUsage(runs, run);
    t.cost_usd = usage.cost_usd;
    t.input_tokens = usage.input_tokens;
    t.output_tokens = usage.output_tokens;
    const now = this.host.clock();
    const end = run.finished_at ? Date.parse(run.finished_at) : now;
    t.finished_at = iso(end);
    const created = Date.parse(run.created_at);
    if (run.finished_at && !Number.isNaN(created)) t.duration_ms = end - created;
    else if ((run.usage?.duration_ms ?? 0) > 0) t.duration_ms = run.usage.duration_ms;
    else if (!Number.isNaN(created)) t.duration_ms = now - created;
  }

  /** Starts one judge run for each scorer of a finished trial that has no result and no judge run. */
  async scoreTrial(trialId: string): Promise<void> {
    const row = this.repo.row();
    if (!row || benchmarkTerminal(row.state)) return;
    const plan = parsePlan(row);
    const t = this.repo.trial(trialId);
    if (!t || t.scored || t.status === "error" || !trialRunFinished(t) || !t.run_id) return;
    const { runs, engine } = this.host.port();
    const run = await runs.getRun(t.run_id);
    if (!run) throw new OfError("resource_not_found", `trial run ${t.run_id} not found`);
    let refs = (await runs.loadStartCommits(run.id)).map((s) => ({
      codeForge: s.codeForge,
      owner: s.owner,
      repo: s.repo,
      ref: s.ref,
    }));
    if (refs.length === 0) refs = t.starting_repo_refs ?? [];
    let changed = false;
    for (const sc of trialScorers(plan, t)) {
      if (trialScore(t, sc.key)) continue;
      const ts: TrialScore = { scorer: sc.key, score: 0, passed: false };
      try {
        const exec: JudgeExec = {};
        if (sc.harness) exec.harness = sc.harness;
        if (sc.machine) exec.machine = sc.machine;
        if (sc.secrets) exec.secrets = sc.secrets;
        if (sc.mcp_servers) exec.mcp_servers = sc.mcp_servers;
        const req = await judgeRequest(runs, {
          factoryId: row.factory_id,
          name: sc.key,
          rubric: sc.rubric,
          labels: sc.labels,
          exec,
          scored: run,
          includeDescendants: true,
          refs,
        });
        req.triggeredBy = "benchmark:" + row.id;
        req.metadata = {
          [META_JUDGE]: JUDGE_TRIAL,
          [META_JUDGE_BENCHMARK]: row.id,
          [META_JUDGE_TRIAL]: t.id,
          [META_JUDGE_KEY]: clip(sc.key, MAX_METADATA_VALUE),
          [META_JUDGE_SCORED_RUN]: run.id,
        };
        // The ID is a hash of (benchmark, trial, scorer): a retry reaches the same judge run.
        const judgeId = await deterministicRunId("trial-judge", row.id, t.id, sc.key);
        await engine.startJudge(req, judgeId);
        ts.judge_run_id = judgeId;
        ts.pending = true;
      } catch (e) {
        ts.error = toOfError(e).error.detail;
      }
      setTrialScore(t, ts);
      changed = true;
    }
    if (!changed) return;
    completeTrialScores(plan, t);
    this.host.persist([t]);
    if (t.scored) await this.releaseTrial(row.id, t);
  }

  /** Ends the session of an idle, blocked or succeeded trial once its evidence is scored. */
  private async releaseTrial(benchId: string, t: Trial): Promise<void> {
    if (!t.run_id || !t.run_state || !(waitsForInput(t.run_state) || t.run_state === "succeeded")) return;
    try {
      await this.host.port().engine.cancel(t.run_id, "benchmark:" + benchId);
    } catch (e) {
      console.warn(`BenchmarkDO: cancel idle trial run ${t.run_id} failed`, e);
    }
  }

  /** A judge run of a trial finished or waits for input: records its answer. */
  async onTrialJudge(judgeRunId: string): Promise<void> {
    const { runs, engine } = this.host.port();
    const judge = await runs.getRun(judgeRunId);
    if (!judge || !finishedForScoring(judge.state)) return;
    const benchId = judge.metadata?.[META_JUDGE_BENCHMARK] ?? "";
    const trialId = judge.metadata?.[META_JUDGE_TRIAL] ?? "";
    if (!benchId || !trialId) return;
    const row = this.repo.row();
    if (!row || row.id !== benchId || benchmarkTerminal(row.state)) return;
    const plan = parsePlan(row);
    const t = this.repo.trial(trialId);
    if (!t) return;
    const ts = (t.scores ?? []).find((s) => s.judge_run_id === judge.id);
    if (!ts || !ts.pending) return;
    const usage = await treeUsage(runs, judge);
    let answer: { label: string; rationale: string } | undefined;
    let jerr = "";
    try {
      answer = await judgeAnswer(runs, judge);
    } catch (e) {
      jerr = e instanceof Error ? e.message : String(e);
    }
    if (!isTerminalState(judge.state)) {
      try {
        await engine.cancel(judge.id, "scoring");
      } catch (e) {
        if (toOfError(e).error.code !== "resource_not_found")
          console.warn(`BenchmarkDO: stop judge ${judge.id} failed`, e);
      }
    }
    const sc: BenchmarkScorer | undefined = trialScorers(plan, t).find((x) => x.key === ts.scorer);
    ts.pending = false;
    ts.judge_cost_usd = usage.cost_usd;
    t.judge_cost_usd = (t.judge_cost_usd ?? 0) + usage.cost_usd;
    if (!sc) ts.error = "unknown scorer";
    else if (!answer) ts.error = jerr;
    else {
      const l = resolveLabel(sc.labels, answer.label);
      if (l) {
        ts.label = l.value;
        ts.score = l.score;
        ts.passed = l.score >= sc.passing_score;
        ts.rationale = answer.rationale;
      } else {
        ts.error = `judge returned undeclared label ${JSON.stringify(answer.label)}`;
      }
    }
    completeTrialScores(plan, t);
    this.host.persist([t]);
    if (t.scored) await this.releaseTrial(row.id, t);
  }

  /**
   * Re-checks every unfinished trial against the runs and scores finished
   * trials (the Go sweep). Returns the errors; one failed trial does not stop
   * the others.
   */
  async reconcileTrials(): Promise<unknown[]> {
    const { runs } = this.host.port();
    const errs: unknown[] = [];
    for (const t of this.repo.trials()) {
      try {
        if (t.status === "running" && t.run_id) {
          const run = await runs.getRun(t.run_id);
          if (run && finishedForScoring(run.state)) await this.onTrialRun(run.id);
        } else if (trialRunFinished(t) && !trialDone(t)) {
          await this.scoreTrial(t.id);
          for (const ts of this.repo.trial(t.id)?.scores ?? []) {
            if (!ts.pending || !ts.judge_run_id) continue;
            const judge = await runs.getRun(ts.judge_run_id);
            if (judge && finishedForScoring(judge.state)) await this.onTrialJudge(judge.id);
          }
        }
      } catch (e) {
        errs.push(e);
      }
    }
    return errs;
  }
}

/** A trial is scored when each of its scorers has a result or an error. */
function completeTrialScores(plan: BenchmarkPlan, t: Trial): void {
  if (t.scored) return;
  for (const sc of trialScorers(plan, t)) {
    const ts = trialScore(t, sc.key);
    if (!ts || ts.pending) return;
  }
  t.scored = true;
}

function setTrialScore(t: Trial, ts: TrialScore): void {
  const list = (t.scores ??= []);
  const i = list.findIndex((s) => s.scorer === ts.scorer);
  if (i >= 0) list[i] = ts;
  else list.push(ts);
}
