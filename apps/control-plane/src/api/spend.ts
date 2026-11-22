// REST handlers: spend. C0 stubs that answer 501. Owner: WS13.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/spend: Model spend of the factory and its team, with the spend limits (owner DO: DispatchDO (counters) + TeamDO (limits)). */
export const getFactorySpend: Handler = notImplementedHandler;
