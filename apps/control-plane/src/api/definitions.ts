// REST handlers: definitions. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/definition: Get the active definition (owner DO: TeamDO). */
export const getDefinition: Handler = notImplementedHandler;

/** PUT /api/v1/factories/{uid}/definition: Validate and apply a definition (managed factories) (owner DO: TeamDO). */
export const putDefinition: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/definition/validate: Validate a definition without applying it (owner DO: TeamDO). */
export const validateDefinition: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/definition/versions: List definition versions (owner DO: TeamDO). */
export const listDefinitionVersions: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/definition/versions/{versionId}: Get a definition version (owner DO: TeamDO). */
export const getDefinitionVersion: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/definition/versions/{versionId}/restore: Re-apply an earlier definition version (owner DO: TeamDO). */
export const restoreDefinitionVersion: Handler = notImplementedHandler;
