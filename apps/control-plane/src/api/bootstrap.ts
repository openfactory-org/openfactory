// REST handler: bootstrap (WS4). POST /bootstrap creates the first team, admin
// and API key, once. TeamDO serializes it (Go used a process mutex).
import { teamApi, type Handler } from "../http/handler";

export const bootstrap: Handler = teamApi();
