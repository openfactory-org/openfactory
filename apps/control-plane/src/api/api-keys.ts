// REST handler of the TeamDO operations that change what an API key may do:
// updateUser (role), deleteAPIKey, updateServiceAccount, deleteServiceAccount
// and deleteServiceAccountKey. Every other users, api-keys and
// service-accounts operation forwards to TeamDO.api directly (src/http/routes.ts).
import { invalidateKeyCache } from "../auth/authenticate";
import { teamApi, type Handler } from "../http/handler";

const forward = teamApi();

/**
 * Forwards to TeamDO.api, then drops this isolate's API key cache after a
 * successful change, so that the change applies at once here.
 */
export const teamDroppingKeyCache: Handler = async (request, env, ctx) => {
  const res = await forward(request, env, ctx);
  if (res.ok) invalidateKeyCache();
  return res;
};
