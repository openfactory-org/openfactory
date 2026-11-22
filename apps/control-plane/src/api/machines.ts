// REST handlers: machines. C0 stubs that answer 501. Owner: WS6.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/machines: List machines declared by factory definitions (owner DO: TeamDO). */
export const listMachines: Handler = notImplementedHandler;
