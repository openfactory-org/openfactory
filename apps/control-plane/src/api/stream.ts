// REST handlers: stream. C0 stubs that answer 501. Owner: WS5.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/stream: Live updates (Server-Sent Events) (owner DO: Worker SSE; push from IndexDO + TeamDO). */
export const streamEvents: Handler = notImplementedHandler;
