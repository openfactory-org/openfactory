// REST handlers: users (WS4). Forwarded to TeamDO.api with the route's operationId.
import { invalidateKeyCache } from "../auth/authenticate";
import { teamApi, type Handler } from "../http/handler";

export const listUsers: Handler = teamApi();
export const createUser: Handler = teamApi();
export const getUser: Handler = teamApi();
/** PATCH /users/{id}: role changes apply at once in this isolate. */
export const updateUser: Handler = async (request, env, ctx) => {
  const res = await teamApi()(request, env, ctx);
  if (res.ok) invalidateKeyCache();
  return res;
};
