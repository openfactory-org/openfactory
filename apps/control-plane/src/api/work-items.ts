// REST handlers: work-items. C0 stubs that answer 501. Owner: WS5.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/work-items: List work items (owner DO: IndexDO (read model)). */
export const listWorkItems: Handler = notImplementedHandler;

/** GET /api/v1/work-items/{id}: Get a work item with timeline and runs (owner DO: WorkDO (owner)). */
export const getWorkItem: Handler = notImplementedHandler;

/** POST /api/v1/work-items/{id}/messages: Message the work item's foreman (owner DO: WorkDO (owner)). */
export const messageWorkItem: Handler = notImplementedHandler;

/** POST /api/v1/work-items/{id}/cancel: Cancel a work item (owner DO: WorkDO (owner)). */
export const cancelWorkItem: Handler = notImplementedHandler;

/** POST /api/v1/work-items/{id}/complete: Complete a work item (owner DO: WorkDO (owner)). */
export const completeWorkItem: Handler = notImplementedHandler;

/** POST /api/v1/work-items/{id}/stage: Move a work item to a stage (owner DO: WorkDO (owner)). */
export const setWorkItemStage: Handler = notImplementedHandler;
