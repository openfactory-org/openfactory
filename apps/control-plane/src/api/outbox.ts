// REST handlers: outbox. C0 stubs that answer 501. Owner: WS5.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/outbox/dead: List dead-lettered events (owner DO: IndexDO (dead-letter index)). */
export const listDeadOutboxMessages: Handler = notImplementedHandler;

/** POST /api/v1/outbox/{id}/redrive: Deliver a dead-lettered event again (owner DO: owning DO (from ID partition token)). */
export const redriveOutboxMessage: Handler = notImplementedHandler;
