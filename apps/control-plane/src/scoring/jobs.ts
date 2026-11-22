// The consumer of the scoring Queue messages. The queue handler in
// src/index.ts calls handleScoringJob for these kinds:
// scoring.runFinished, scoring.sample, judge.result, bench.trialUpdate.
import { DEFAULT_TEAM, doName } from "@openfactory/core";
import type { JobMessage } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { remoteStore, workEngine, workRunsPort } from "./clients";
import { ScoringService, type BenchmarkPort } from "./service";
import type { ScoringOptions } from "./types";
import { benchmarkDoName } from "./names";

export const SCORING_JOB_KINDS = [
  "scoring.runFinished",
  "scoring.sample",
  "judge.result",
  "bench.trialUpdate",
] as const;

export function benchmarkPort(env: Env, teamId: string): BenchmarkPort {
  const stub = (benchId: string) => env.BENCHMARK.getByName(benchmarkDoName(teamId, benchId));
  return {
    trialRunUpdate: (benchId, runId) => stub(benchId).trialRunUpdate(runId),
    // The judge run carries the benchmark ID in its metadata; the DO reads it.
    trialJudgeUpdate: (benchId, judgeRunId) => stub(benchId).trialJudgeUpdate(judgeRunId),
  };
}

export function scoringOptions(env: Env): ScoringOptions {
  const opt = (v: string | undefined) => (v ?? "").trim();
  return {
    publicUrl: env.OF_PUBLIC_URL,
    ...(opt(env.OF_CORRECTNESS_MODEL) ? { correctnessModel: opt(env.OF_CORRECTNESS_MODEL) } : {}),
    ...(opt(env.OF_ANALYSIS_MODEL) ? { analysisModel: opt(env.OF_ANALYSIS_MODEL) } : {}),
    ...(opt(env.OF_IMPROVEMENT_AGENT) ? { improvementAgent: opt(env.OF_IMPROVEMENT_AGENT) } : {}),
  };
}

export function scoringService(env: Env, teamId: string = DEFAULT_TEAM): ScoringService {
  return new ScoringService({
    store: remoteStore(env, teamId),
    runs: workRunsPort(env, teamId),
    engine: workEngine(env, teamId),
    benchmarks: benchmarkPort(env, teamId),
    opts: scoringOptions(env),
  });
}

/** Handles one scoring job. Errors make the queue retry the message; every step is idempotent. */
export async function handleScoringJob(env: Env, msg: JobMessage, teamId: string = DEFAULT_TEAM): Promise<void> {
  switch (msg.kind) {
    case "scoring.runFinished":
    case "scoring.sample":
      await scoringService(env, teamId).handleRunUpdate(msg.runId);
      return;
    case "judge.result":
      // The judge run itself is the unit: its update finishes the score.
      await scoringService(env, teamId).handleRunUpdate(
        String((msg.result as { judge_run_id?: string }).judge_run_id ?? msg.runId),
      );
      return;
    case "bench.trialUpdate":
      await env.BENCHMARK.getByName(benchmarkDoName(teamId, msg.benchId)).trialRunUpdate(msg.runId);
      return;
    default:
      return;
  }
}

export { doName };
