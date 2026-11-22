// REST handlers: bootstrap. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** POST /api/v1/bootstrap: Create the first team, admin and API key (one-time) (owner DO: TeamDO). */
export const bootstrap: Handler = notImplementedHandler;
