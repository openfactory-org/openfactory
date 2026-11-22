// REST handlers: teams. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/teams: List teams (your team) (owner DO: TeamDO). */
export const listTeams: Handler = notImplementedHandler;

/** GET /api/v1/teams/{id}: Get team (owner DO: TeamDO). */
export const getTeam: Handler = notImplementedHandler;
