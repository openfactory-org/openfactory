// The scoring service: the entry point of the scoring jobs and the API. It
// routes a run update to the part that owns it:
// - ScoringEvaluator (evaluator.ts): sampling, judge runs and scores of
//   production runs;
// - SelfImprovementCoordinator (self-improvement.ts): failure grouping,
//   improvement runs and the review of their pull requests;
// - BenchmarkDO: trials of a benchmark and their judge runs.
//
// Differences from Go, by design: no in-process locks and no direct model
// call. DO serialization and idempotent calls replace the locks. The state of
// a service call is in TeamDO (score rows, markers), reached through the
// ScoringStore port.
import type { Run, Score } from "@openfactory/core";
import { ScoringContext } from "./context";
import { ScoringEvaluator } from "./evaluator";
import type { Failure } from "./improve";
import { SelfImprovementCoordinator } from "./self-improvement";
import {
  JUDGE_SCORE,
  JUDGE_TRIAL,
  META_BENCHMARK_RUN,
  META_JUDGE,
  META_JUDGE_BENCHMARK,
  META_SELF_IMPROVEMENT,
  finishedForScoring,
  type EnginePort,
  type RunsPort,
  type ScoringOptions,
  type ScoringStore,
} from "./types";

// do/team/factories.ts reads the track of a Self-improvement run.
export { improveTrackKey, type ImprovementTrack } from "./self-improvement";

/** What the service tells BenchmarkDO. */
export interface BenchmarkPort {
  /** A root run of the benchmark finished or waits for input. */
  trialRunUpdate(benchId: string, runId: string): Promise<void>;
  /** A judge run of a trial finished or waits for input. */
  trialJudgeUpdate(benchId: string, judgeRunId: string): Promise<void>;
}

export interface ScoringDeps {
  store: ScoringStore;
  runs: RunsPort;
  engine: EnginePort;
  benchmarks: BenchmarkPort;
  opts?: ScoringOptions;
}

export class ScoringService {
  private readonly runs: RunsPort;
  private readonly benchmarks: BenchmarkPort;
  private readonly ctx: ScoringContext;
  readonly evaluator: ScoringEvaluator;
  readonly improvement: SelfImprovementCoordinator;

  constructor(deps: ScoringDeps) {
    this.runs = deps.runs;
    this.benchmarks = deps.benchmarks;
    this.ctx = new ScoringContext(deps.opts);
    const { store, runs, engine } = deps;
    const ctx = this.ctx;
    this.improvement = new SelfImprovementCoordinator({ store, runs, engine, ctx });
    this.evaluator = new ScoringEvaluator({
      store,
      runs,
      engine,
      ctx,
      onFailedScore: (factoryId, agent) => this.improvement.checkAgent(factoryId, agent, false),
    });
  }

  /**
   * Processes the end of a turn or a run (the `scoring.runFinished` job): it
   * advances a benchmark the run is a trial of, reviews the pull requests of a
   * Self-improvement run, and scores production runs.
   */
  async handleRunUpdate(runId: string): Promise<void> {
    const run = await this.runs.getRun(runId);
    if (!run || !finishedForScoring(run.state)) return;
    const md = run.metadata ?? {};
    if (md[META_JUDGE]) {
      // A judge run is never scored itself.
      await this.handleJudgeUpdate(run);
      return;
    }
    if (md[META_SELF_IMPROVEMENT] === "true" && run.work_item_id) {
      try {
        await this.improvement.reviewImprovementPRs(run);
      } catch (e) {
        this.ctx.log("error", "self-improvement review of pull requests", { run_id: run.id, err: String(e) });
      }
    }
    const bid = md[META_BENCHMARK_RUN];
    if (bid) {
      // Root trial runs carry the benchmark id; descendants inherit metadata and
      // are ignored by BenchmarkDO (unknown run ID).
      await this.benchmarks.trialRunUpdate(bid, run.id);
      return;
    }
    if (run.state === "error") return; // scorers ignore runs in error
    await this.evaluator.autoScore(run);
  }

  private async handleJudgeUpdate(judge: Run): Promise<void> {
    switch (judge.metadata?.[META_JUDGE]) {
      case JUDGE_SCORE:
        await this.evaluator.finishScoreJudge(judge);
        return;
      case JUDGE_TRIAL: {
        const bid = judge.metadata[META_JUDGE_BENCHMARK];
        if (bid) await this.benchmarks.trialJudgeUpdate(bid, judge.id);
        return;
      }
    }
  }

  /** Scores one run on demand. See ScoringEvaluator.scoreRun. */
  scoreRun(scorerIdOrSlug: string, runId: string): Promise<Score> {
    return this.evaluator.scoreRun(scorerIdOrSlug, runId);
  }

  /** The scheduled check of one factory. See SelfImprovementCoordinator.checkSelfImprovement. */
  checkSelfImprovement(factoryId: string): Promise<Run[]> {
    return this.improvement.checkSelfImprovement(factoryId);
  }

  /** "Run now". See SelfImprovementCoordinator.triggerSelfImprovement. */
  triggerSelfImprovement(factoryId: string, agent: string): Promise<Run[]> {
    return this.improvement.triggerSelfImprovement(factoryId, agent);
  }

  /** Unreviewed failures per agent. */
  unreviewedFailures(factoryId: string): Promise<Map<string, Failure[]>> {
    return this.improvement.unreviewedFailures(factoryId);
  }
}
