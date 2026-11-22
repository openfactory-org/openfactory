// REST handlers: version. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/version: Server build version (owner DO: Worker (static)). */
export const getVersion: Handler = notImplementedHandler;
