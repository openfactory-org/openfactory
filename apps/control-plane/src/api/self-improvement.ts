// REST handlers: self-improvement. Owner: WS13.
import { json, type Handler } from "../http/handler";
import { scoringService } from "../scoring/jobs";
import { factoryAccess } from "./measure";

/** POST /api/v1/factories/{uid}/self-improvement: Start self-improvement now. */
export const runSelfImprovement: Handler = async (_req, env, ctx) => {
  const f = await factoryAccess(env, ctx);
  const notStarted = (reason: string) => json({ factory_id: f.factory_id, started: false, run_ids: [], reason });
  if (f.scorers.length === 0) return notStarted("The factory has no scorers. Add a scorer with selfImprovement turned on.");
  if (!f.scorers.some((s) => s.self_improvement)) return notStarted("No scorer of the factory has selfImprovement turned on.");
  const runs = await scoringService(env, f.team_id).triggerSelfImprovement(f.factory_id, "");
  if (runs.length === 0) return notStarted("No failed scores wait for review.");
  return json({ factory_id: f.factory_id, started: true, run_ids: runs.map((r) => r.id) }, 202);
};
