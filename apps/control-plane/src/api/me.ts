// REST handler: GET /me.
import { loginProviders } from "../auth/routes";
import { callTeam, json, type Handler } from "../http/handler";

export const getMe: Handler = async (_request, env, ctx) => {
  const res = await callTeam(env, ctx, "getMe", undefined, ctx.principal?.keyId ? { "?auth": "api_key" } : {});
  const body = (await res.json()) as Record<string, unknown>;
  return json({ ...body, login_providers: loginProviders(env) });
};
