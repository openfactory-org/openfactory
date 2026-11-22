// The scheduled check of Self-improvement (the Go sweep, every 15 minutes).
import type { Env } from "../contracts/env";
import { workEngine, workRunsPort } from "./clients";
import { benchmarkPort, scoringOptions } from "./jobs";
import { ScoringService } from "./service";
import type { ScoringStore } from "./types";

/**
 * Runs the check for every factory that has a failed score of a
 * self-improvement scorer. `store` is the local store of TeamDO. Returns true
 * when it should run again.
 */
export async function scoringSweep(env: Env, teamId: string, store: ScoringStore, factoryIds: string[] = []): Promise<boolean> {
  const svc = new ScoringService({ store, runs: workRunsPort(env, teamId), engine: workEngine(env, teamId), benchmarks: benchmarkPort(env, teamId), opts: scoringOptions(env) });
  let pending = false;
  for (const id of factoryIds) {
    try {
      await svc.checkSelfImprovement(id);
      if ((await svc.unreviewedFailures(id)).size > 0) pending = true;
    } catch (e) {
      console.error("scoring sweep", id, e);
      pending = true;
    }
  }
  return pending;
}
