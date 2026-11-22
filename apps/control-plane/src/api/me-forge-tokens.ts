// REST handlers: me-forge-tokens. C0 stubs that answer 501. Owner: WS10.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/me/forge-tokens: List the code forges for which you stored a personal token (owner DO: TeamDO). */
export const listUserForgeTokens: Handler = notImplementedHandler;

/** PUT /api/v1/me/forge-tokens/{forge}: Store your personal code-forge token (used by runs with credentialStrategy CREATOR) (owner DO: TeamDO). */
export const setUserForgeToken: Handler = notImplementedHandler;

/** DELETE /api/v1/me/forge-tokens/{forge}: Delete your personal code-forge token (owner DO: TeamDO). */
export const deleteUserForgeToken: Handler = notImplementedHandler;
