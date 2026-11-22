// REST handlers: schedules. C0 stubs that answer 501. Owner: WS10.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/schedules: List the schedules of the factory with their last and next run (owner DO: TeamDO). */
export const listSchedules: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/schedules/{id}/pause: Pause a schedule without a definition change (owner DO: TeamDO). */
export const pauseSchedule: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/schedules/{id}/resume: Resume a paused schedule (owner DO: TeamDO). */
export const resumeSchedule: Handler = notImplementedHandler;

/** POST /api/v1/automations/{id}/run: Start an automation manually (owner DO: TeamDO → IngressDO → WorkDO). */
export const runAutomation: Handler = notImplementedHandler;
