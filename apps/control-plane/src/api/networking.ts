// REST handlers: networking. C0 stubs that answer 501. Owner: WS4.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/networking/policies: Built-in network policy catalog (owner DO: Worker (static)). */
export const listNetworkPolicies: Handler = notImplementedHandler;
