// REST handlers: environments. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/environments: List environments (owner DO: TeamDO). */
export const listEnvironments: Handler = notImplementedHandler;

/** POST /api/v1/environments: Create an environment (owner DO: TeamDO). */
export const createEnvironment: Handler = notImplementedHandler;

/** GET /api/v1/environments/{id}: Get an environment (owner DO: TeamDO). */
export const getEnvironment: Handler = notImplementedHandler;

/** PATCH /api/v1/environments/{id}: Update an environment (owner DO: TeamDO). */
export const updateEnvironment: Handler = notImplementedHandler;

/** DELETE /api/v1/environments/{id}: Delete an environment (owner DO: TeamDO). */
export const deleteEnvironment: Handler = notImplementedHandler;
