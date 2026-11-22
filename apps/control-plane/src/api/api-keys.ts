// REST handlers: api-keys. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/api-keys: List API keys (own keys; admins see all) (owner DO: TeamDO). */
export const listAPIKeys: Handler = notImplementedHandler;

/** POST /api/v1/api-keys: Create an API key (owner DO: TeamDO). */
export const createAPIKey: Handler = notImplementedHandler;

/** DELETE /api/v1/api-keys/{id}: Revoke an API key (owner DO: TeamDO). */
export const deleteAPIKey: Handler = notImplementedHandler;
