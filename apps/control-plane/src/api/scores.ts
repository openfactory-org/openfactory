// REST handlers: scores. C0 stubs that answer 501. Owner: WS13.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/scorers: List scorers (owner DO: TeamDO). */
export const listScorers: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/scores: List scores (owner DO: IndexDO (read model)). */
export const listScores: Handler = notImplementedHandler;

/** POST /api/v1/runs/{runId}/score: Score a run with a scorer now (owner DO: new WorkDO (judge run, key `run:<id>`)). */
export const scoreRun: Handler = notImplementedHandler;
