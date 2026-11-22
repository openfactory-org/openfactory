// REST handlers: secrets. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/secrets: List secrets (values are never returned) (owner DO: TeamDO). */
export const listSecrets: Handler = notImplementedHandler;

/** POST /api/v1/secrets: Create a secret (owner DO: TeamDO). */
export const createSecret: Handler = notImplementedHandler;

/** POST /api/v1/secrets/rewrap: Encrypt all secrets again with the current key. */
export const rewrapSecrets: Handler = notImplementedHandler;

/** GET /api/v1/secrets/{name}: Get secret metadata (owner DO: TeamDO). */
export const getSecret: Handler = notImplementedHandler;

/** PUT /api/v1/secrets/{name}: Replace a secret's value (owner DO: TeamDO). */
export const updateSecret: Handler = notImplementedHandler;

/** DELETE /api/v1/secrets/{name}: Delete a secret (owner DO: TeamDO). */
export const deleteSecret: Handler = notImplementedHandler;
