// REST handlers: teams (WS4). Forwarded to TeamDO.api with the route's operationId.
import { teamApi, type Handler } from "../http/handler";

export const listTeams: Handler = teamApi();
export const getTeam: Handler = teamApi();
