// REST handlers: machines (WS6). GET /machines lists the machines declared by
// the active definitions of the team's factories (port of handleListMachines).
import { teamApi, type Handler } from "../http/handler";

/** GET /api/v1/machines: List machines declared by factory definitions (owner DO: TeamDO). */
export const listMachines: Handler = teamApi();
