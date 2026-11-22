// REST handlers: service-accounts. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/service-accounts: List the service accounts of the team (owner DO: TeamDO). */
export const listServiceAccounts: Handler = notImplementedHandler;

/** POST /api/v1/service-accounts: Create a service account (owner DO: TeamDO). */
export const createServiceAccount: Handler = notImplementedHandler;

/** GET /api/v1/service-accounts/{id}: Get a service account (owner DO: TeamDO). */
export const getServiceAccount: Handler = notImplementedHandler;

/** PATCH /api/v1/service-accounts/{id}: Change the name, role or factory roles of a service account (owner DO: TeamDO). */
export const updateServiceAccount: Handler = notImplementedHandler;

/** DELETE /api/v1/service-accounts/{id}: Delete a service account and its API keys (owner DO: TeamDO). */
export const deleteServiceAccount: Handler = notImplementedHandler;

/** GET /api/v1/service-accounts/{id}/keys: List the API keys of a service account (owner DO: TeamDO). */
export const listServiceAccountKeys: Handler = notImplementedHandler;

/** POST /api/v1/service-accounts/{id}/keys: Create an API key (ofa_) for a service account (owner DO: TeamDO). */
export const createServiceAccountKey: Handler = notImplementedHandler;

/** DELETE /api/v1/service-accounts/{id}/keys/{keyId}: Revoke an API key of a service account (owner DO: TeamDO). */
export const deleteServiceAccountKey: Handler = notImplementedHandler;
