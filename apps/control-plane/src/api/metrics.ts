// REST handlers: metrics. C0 stubs that answer 501. Owner: WS13.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/metrics: Dashboard metrics (owner DO: IndexDO (read model)). */
export const getMetrics: Handler = notImplementedHandler;
