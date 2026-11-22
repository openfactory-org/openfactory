// REST handlers: personal forge tokens (owner WS10a). TeamDO module
// src/do/team/schedules.ts stores them sealed with the secret-store key.
import { teamApi, type Handler } from "../http/handler";

/** GET /api/v1/me/forge-tokens */
export const listUserForgeTokens: Handler = teamApi();
/** PUT /api/v1/me/forge-tokens/{forge} */
export const setUserForgeToken: Handler = teamApi();
/** DELETE /api/v1/me/forge-tokens/{forge} */
export const deleteUserForgeToken: Handler = teamApi();
