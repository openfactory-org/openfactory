// REST handlers: service-accounts (WS4). Forwarded to TeamDO.api with the route's operationId.
import { invalidateKeyCache } from "../auth/authenticate";
import { teamApi, type Handler } from "../http/handler";

/** Forwards and drops this isolate's key cache after a successful change. */
const revoking = (): Handler => async (request, env, ctx) => {
  const res = await teamApi()(request, env, ctx);
  if (res.ok) invalidateKeyCache();
  return res;
};

export const listServiceAccounts: Handler = teamApi();
export const createServiceAccount: Handler = teamApi();
export const getServiceAccount: Handler = teamApi();
export const updateServiceAccount: Handler = revoking();
export const deleteServiceAccount: Handler = revoking();
export const listServiceAccountKeys: Handler = teamApi();
export const createServiceAccountKey: Handler = teamApi();
export const deleteServiceAccountKey: Handler = revoking();
