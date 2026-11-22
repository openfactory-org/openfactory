// REST handlers: integrations. C0 stubs that answer 501. Owner: WS10.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/integrations: List integrations (owner DO: TeamDO). */
export const listIntegrations: Handler = notImplementedHandler;

/** POST /api/v1/integrations: Create an integration (owner DO: TeamDO). */
export const createIntegration: Handler = notImplementedHandler;

/** GET /api/v1/integrations/{id}: Get an integration (owner DO: TeamDO). */
export const getIntegration: Handler = notImplementedHandler;

/** PATCH /api/v1/integrations/{id}: Update an integration (owner DO: TeamDO). */
export const updateIntegration: Handler = notImplementedHandler;

/** DELETE /api/v1/integrations/{id}: Delete an integration (owner DO: TeamDO). */
export const deleteIntegration: Handler = notImplementedHandler;
