// REST handlers: integrations (WS10b). Forwarded to TeamDO.api with the route's operationId.
import { teamApi, type Handler } from "../http/handler";

/** GET /api/v1/integrations: List integrations (owner DO: TeamDO). */
export const listIntegrations: Handler = teamApi();

/** POST /api/v1/integrations: Create an integration (owner DO: TeamDO). */
export const createIntegration: Handler = teamApi();

/** GET /api/v1/integrations/{id}: Get an integration (owner DO: TeamDO). */
export const getIntegration: Handler = teamApi();

/** PATCH /api/v1/integrations/{id}: Update an integration (owner DO: TeamDO). */
export const updateIntegration: Handler = teamApi();

/** DELETE /api/v1/integrations/{id}: Delete an integration (owner DO: TeamDO). */
export const deleteIntegration: Handler = teamApi();
