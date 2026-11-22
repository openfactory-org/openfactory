// Scoring of production runs: sampling, judge runs and score rows. Port of
// scorer.go and judge.go.
//
// No in-process lock: a judge run has an ID that is a hash of (scorer, run,
// attempt), a marker in TeamDO records the attempt, and the store upserts a
// score by (scorer, run). A repeated update starts no second judge run.
import { OfError, isOfError, type Run, type Score, type Scorer, type Usage } from "@openfactory/core";
import { errMsg, type ScoringContext } from "./context";
import { deterministicRunId, isJudgeRun, judgeAnswer, judgeRequest, scorerExec, scorerName, treeUsage } from "./judge";
import { resolveLabel, shouldSample } from "./sample";
import {
  JUDGE_SCORE,
  MAX_ANCESTOR_DEPTH,
  META_BENCHMARK_RUN,
  META_JUDGE,
  META_JUDGE_SCORED_RUN,
  META_JUDGE_SCORER,
  TRIGGERED_BY_SCORER,
  finishedForScoring,
  isTerminalState,
  type EnginePort,
  type RunsPort,
  type ScoringStore,
} from "./types";

/** The marker of the judge run of (scorer, run). `attempt` only grows, so a judge run ID is never reused. */
export interface JudgeMarker {
  attempt: number;
  run_id: string;
  active: boolean;
}

export const pendingJudgeKey = (scorerId: string, runId: string): string => `scoring:judge:${scorerId}:${runId}`;

export interface ScoringEvaluatorDeps {
  store: ScoringStore;
  runs: RunsPort;
  engine: EnginePort;
  ctx: ScoringContext;
  /** Called when a scorer with Self-improvement records a failed score for an agent. */
  onFailedScore: (factoryId: string, agent: string) => Promise<unknown>;
}

export class ScoringEvaluator {
  private readonly store: ScoringStore;
  private readonly runs: RunsPort;
  private readonly engine: EnginePort;
  private readonly ctx: ScoringContext;
  private readonly onFailedScore: ScoringEvaluatorDeps["onFailedScore"];

  constructor(deps: ScoringEvaluatorDeps) {
    this.store = deps.store;
    this.runs = deps.runs;
    this.engine = deps.engine;
    this.ctx = deps.ctx;
    this.onFailedScore = deps.onFailedScore;
  }

  // ---------- automatic scoring ----------

  /** Starts the judge of every scorer that selects the run's agent and samples the run. */
  async autoScore(run: Run): Promise<void> {
    if (run.state !== "succeeded" && run.state !== "failed") return;
    if (!run.agent_name || isJudgeRun(run)) return;
    const scorers = await this.store.listScorers(run.factory_id);
    const eligible: Scorer[] = [];
    for (const sc of scorers) {
      if (!(sc.agents ?? []).some((a) => a.name === run.agent_name)) continue;
      if (!(await shouldSample(sc.id, run.id, sc.sampling_rate))) continue;
      eligible.push(sc);
    }
    if (eligible.length === 0) return;
    // Benchmark trials (and their descendants) are scored by the benchmark.
    if (await this.isBenchmarkRun(run)) return;
    // Every score comes from a judge run, so scoreOnce stores nothing here.
    // finishScoreJudge stores the score and runs the self-improvement check
    // for a failed one. (Go checked here only for scores that a classifier
    // gave inline, without a judge run; the judge path checked later too.)
    const errs: string[] = [];
    for (const sc of eligible) {
      try {
        await this.scoreOnce(sc, run, false);
      } catch (e) {
        errs.push(`scorer ${sc.slug}: ${errMsg(e)}`);
      }
    }
    if (errs.length > 0) throw new OfError("internal_error", errs.join("; "));
  }

  /** Reports whether the run or any ancestor was started by a benchmark. */
  async isBenchmarkRun(run: Run): Promise<boolean> {
    let cur: Run = run;
    for (let depth = 0; depth < MAX_ANCESTOR_DEPTH; depth++) {
      if ((cur.triggered_by ?? "").startsWith("benchmark:") || cur.metadata?.[META_BENCHMARK_RUN]) return true;
      if (!cur.parent_run_id) return false;
      const parent = await this.runs.getRun(cur.parent_run_id);
      if (!parent) return false;
      cur = parent;
    }
    return false;
  }

  /**
   * Scores one run on demand with the given scorer (ID or slug), whatever the
   * sampling and whether the scorer selects the run's agent. A new result
   * replaces the previous one of that scorer. The result is a pending score
   * without an ID (judge_run_id names the judge run); the score is stored when
   * the judge run finishes.
   */
  async scoreRun(scorerIdOrSlug: string, runId: string): Promise<Score> {
    const run = await this.runs.getRun(runId);
    if (!run) throw new OfError("resource_not_found", `run ${runId} not found`);
    const sc = await this.findScorer(run.factory_id, scorerIdOrSlug);
    if (isJudgeRun(run)) throw new OfError("invalid_request", "a judge run is not scored");
    // A pending score: finishScoreJudge stores it and runs the self-improvement check.
    return this.scoreOnce(sc, run, true);
  }

  async findScorer(factoryId: string, idOrSlug: string): Promise<Scorer> {
    const scorers = await this.store.listScorers(factoryId);
    const sc =
      scorers.find((s) => s.id === idOrSlug) ?? scorers.find((s) => s.slug === idOrSlug || s.name === idOrSlug);
    if (!sc) throw new OfError("resource_not_found", `scorer ${JSON.stringify(idOrSlug)} not found`);
    return sc;
  }

  private async getMarker(scorerId: string, runId: string): Promise<JudgeMarker | undefined> {
    const v = await this.store.kvGet(pendingJudgeKey(scorerId, runId));
    if (!v) return undefined;
    try {
      return JSON.parse(v) as JudgeMarker;
    } catch {
      return undefined;
    }
  }

  private async setMarker(scorerId: string, runId: string, m: JudgeMarker): Promise<void> {
    await this.store.kvSet(pendingJudgeKey(scorerId, runId), JSON.stringify(m));
  }

  /**
   * Starts the judge of (scorer, run) unless a result exists (replace = false)
   * or a judge works. Returns the existing score, or a pending score (no ID).
   */
  private async scoreOnce(sc: Scorer, run: Run, replace: boolean): Promise<Score> {
    const existing = await this.store.getScore(sc.id, run.id);
    if (existing && !replace) return existing;
    const marker = await this.getMarker(sc.id, run.id);
    let judgeId: string;
    const pending = marker?.active ? await this.runs.getRun(marker.run_id) : null;
    if (marker?.active && (!pending || !finishedForScoring(pending.state))) {
      // A judge works, or its start did not finish: start (or continue) the same run.
      judgeId = marker.run_id;
      if (!pending) await this.startScoreJudge(sc, run, judgeId);
    } else {
      // No judge, or a finished judge whose update was lost: a new judge run.
      const attempt = (marker?.attempt ?? 0) + 1;
      judgeId = await deterministicRunId("judge", sc.id, run.id, String(attempt));
      await this.setMarker(sc.id, run.id, { attempt, run_id: judgeId, active: true });
      await this.startScoreJudge(sc, run, judgeId);
    }
    return {
      id: "",
      scorer_id: sc.id,
      run_id: run.id,
      agent_name: run.agent_name ?? "",
      label: "",
      score: 0,
      passed: false,
      rationale: "",
      created_at: "",
      judge_run_id: judgeId,
    };
  }

  private async startScoreJudge(sc: Scorer, run: Run, judgeId: string): Promise<void> {
    const include = (sc.agents ?? []).find((a) => a.name === run.agent_name)?.includeDescendants ?? false;
    const req = await judgeRequest(this.runs, {
      factoryId: run.factory_id,
      name: scorerName(sc),
      rubric: sc.rubric,
      labels: sc.labels ?? [],
      exec: scorerExec(sc),
      scored: run,
      includeDescendants: include,
    });
    req.triggeredBy = TRIGGERED_BY_SCORER + sc.id;
    req.metadata = { [META_JUDGE]: JUDGE_SCORE, [META_JUDGE_SCORER]: sc.id, [META_JUDGE_SCORED_RUN]: run.id };
    await this.engine.startJudge(req, judgeId);
    this.ctx.log("info", "judge run started", { scorer: sc.slug, run_id: run.id, judge_run_id: judgeId });
  }

  // ---------- judge results ----------

  /** Stores the score of a finished judge run of a scorer, unless a newer judge replaced it. */
  async finishScoreJudge(judge: Run): Promise<void> {
    const scorerId = judge.metadata?.[META_JUDGE_SCORER] ?? "";
    const runId = judge.metadata?.[META_JUDGE_SCORED_RUN] ?? "";
    if (!scorerId || !runId) return;
    const marker = await this.getMarker(scorerId, runId);
    if (!marker || !marker.active || marker.run_id !== judge.id) return; // stored already, or a newer judge replaced this one
    const scored = await this.runs.getRun(runId);
    if (!scored) throw new OfError("resource_not_found", `run ${runId} not found`);
    let sc: Scorer;
    try {
      sc = await this.findScorer(scored.factory_id, scorerId);
    } catch (e) {
      await this.setMarker(scorerId, runId, { ...marker, active: false });
      throw e;
    }
    const usage = await this.treeUsage(judge);
    let answer: { label: string; rationale: string } | undefined;
    let jerr: string | undefined;
    try {
      answer = await this.judgeAnswer(judge);
    } catch (e) {
      jerr = errMsg(e);
    }
    await this.stopJudge(judge);
    if (!answer) {
      await this.setMarker(scorerId, runId, { ...marker, active: false });
      this.ctx.log("error", "judge run gave no label", {
        scorer: sc.slug,
        run_id: runId,
        judge_run_id: judge.id,
        err: jerr,
      });
      return;
    }
    const l = resolveLabel(sc.labels, answer.label);
    if (!l) {
      await this.setMarker(scorerId, runId, { ...marker, active: false });
      this.ctx.log("error", "judge run returned an undeclared label", {
        scorer: sc.slug,
        run_id: runId,
        judge_run_id: judge.id,
        label: answer.label,
      });
      return;
    }
    const existing = await this.store.getScore(sc.id, runId);
    const score: Score & { factory_id: string } = {
      id: existing?.id ?? this.ctx.newId("score"),
      factory_id: scored.factory_id,
      scorer_id: sc.id,
      run_id: runId,
      agent_name: scored.agent_name ?? "",
      label: l.value,
      score: l.score,
      passed: l.score >= sc.passing_score,
      rationale: answer.rationale,
      created_at: this.ctx.nowIso(),
      judge_run_id: judge.id,
      judge_usage: usage,
    };
    if (existing?.improvement_run_id) score.improvement_run_id = existing.improvement_run_id;
    await this.store.putScore(score);
    await this.setMarker(scorerId, runId, { ...marker, active: false });
    this.ctx.log("info", "run scored", {
      scorer: sc.slug,
      run_id: runId,
      judge_run_id: judge.id,
      label: score.label,
      passed: score.passed,
    });
    if (!score.passed && sc.self_improvement && scored.agent_name) {
      try {
        await this.onFailedScore(scored.factory_id, scored.agent_name);
      } catch (e) {
        this.ctx.log("error", "self-improvement check after judge run", { run_id: runId, err: String(e) });
      }
    }
  }

  /** Cancels a judge run that waits for input. A judge has one turn. */
  private async stopJudge(judge: Run): Promise<void> {
    if (isTerminalState(judge.state)) return;
    try {
      await this.engine.cancel(judge.id, "scoring");
    } catch (e) {
      if (!(isOfError(e) && e.code === "resource_not_found"))
        this.ctx.log("warn", "stop judge run", { judge_run_id: judge.id, err: String(e) });
    }
  }

  /** Reads the label and rationale from a finished judge run. */
  judgeAnswer(judge: Run): Promise<{ label: string; rationale: string }> {
    return judgeAnswer(this.runs, judge);
  }

  /** The usage of a run and its descendants. */
  treeUsage(run: Run): Promise<Usage> {
    return treeUsage(this.runs, run);
  }
}
