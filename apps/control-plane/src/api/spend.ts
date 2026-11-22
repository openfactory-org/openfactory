// REST handlers: spend (reads from DispatchDO, limits from TeamDO).
import { doName } from "@openfactory/core";
import { json, teamOf, type Handler } from "../http/handler";
import { factoryAccess } from "./measure";

/** GET /api/v1/factories/{uid}/spend: Model spend of the factory and its team, with the limits. */
export const getFactorySpend: Handler = async (_req, env, ctx) => {
  const f = await factoryAccess(env, ctx);
  const out = await env.DISPATCH.getByName(doName.dispatch(teamOf(ctx))).spend(f.factory_id);
  return json(out);
};
