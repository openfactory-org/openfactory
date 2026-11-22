// REST handlers: me. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/me: Current principal (owner DO: TeamDO). */
export const getMe: Handler = notImplementedHandler;
