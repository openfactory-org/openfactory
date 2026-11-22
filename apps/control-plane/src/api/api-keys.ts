// REST handlers: api-keys (WS4). Forwarded to TeamDO.api with the route's operationId.
import { invalidateKeyCache } from "../auth/authenticate";
import { teamApi, type Handler } from "../http/handler";

export const listAPIKeys: Handler = teamApi();
export const createAPIKey: Handler = teamApi();

/** DELETE /api-keys/{id}: also drops this isolate's key cache, so the key stops working at once here. */
export const deleteAPIKey: Handler = async (request, env, ctx) => {
  const res = await teamApi()(request, env, ctx);
  if (res.ok) invalidateKeyCache();
  return res;
};
