// REST handlers: inbox. C0 stubs that answer 501. Owner: WS5.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/inbox: Your inbox across the team's factories (owner DO: IndexDO (read model)). */
export const listTeamInbox: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/inbox: Factory inbox (owner DO: IndexDO (read model)). */
export const listInbox: Handler = notImplementedHandler;

/** GET /api/v1/inbox/{id}: Get an inbox item (owner DO: WorkDO (owner)). */
export const getInboxItem: Handler = notImplementedHandler;

/** POST /api/v1/inbox/{id}/resolve: Resolve an inbox item (owner DO: WorkDO (owner)). */
export const resolveInboxItem: Handler = notImplementedHandler;

/** POST /api/v1/inbox/{id}/read: Mark an inbox item as read (owner DO: IndexDO (per-user read state)). */
export const readInboxItem: Handler = notImplementedHandler;

/** POST /api/v1/inbox/{id}/unresolve: Undo a resolve (owner DO: WorkDO (owner)). */
export const unresolveInboxItem: Handler = notImplementedHandler;
