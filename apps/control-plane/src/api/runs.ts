// REST handlers: runs. C0 stubs that answer 501. Owner: WS5.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** POST /api/v1/factories/{uid}/runs: Dispatch a run to the factory's foreman (owner DO: TeamDO read → new WorkDO (owner)). */
export const dispatchRun: Handler = notImplementedHandler;

/** GET /api/v1/runs: List runs (owner DO: IndexDO (read model)). */
export const listRuns: Handler = notImplementedHandler;

/** POST /api/v1/runs: Start a run of an agent of a factory (owner DO: TeamDO read → new WorkDO (owner)). */
export const createRun: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}: Get a run (owner DO: WorkDO (owner)). */
export const getRun: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/messages: List run messages (owner DO: WorkDO (owner)). */
export const listRunMessages: Handler = notImplementedHandler;

/** POST /api/v1/runs/{runId}/messages: Send a message to a run (owner DO: WorkDO (owner)). */
export const postRunMessage: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/events: Stream run events (Server-Sent Events) (owner DO: WorkDO (owner)). */
export const streamRunEvents: Handler = notImplementedHandler;

/** POST /api/v1/runs/{runId}/followups: Send a follow-up message (owner DO: WorkDO (owner)). */
export const followUpRun: Handler = notImplementedHandler;

/** POST /api/v1/runs/{runId}/cancel: Cancel a run (owner DO: WorkDO (owner)). */
export const cancelRun: Handler = notImplementedHandler;

/** POST /api/v1/runs/{runId}/interrupt: Stop the active turn of a run and keep its session (owner DO: WorkDO (owner)). */
export const interruptRun: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/transcript: Download the run transcript (owner DO: WorkDO (owner)). */
export const getRunTranscript: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/artifacts: List run artifacts (owner DO: WorkDO (owner) + R2). */
export const listRunArtifacts: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/artifacts/{name}: Download a run artifact (owner DO: WorkDO (owner) + R2). */
export const getRunArtifact: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/attachments: List prompt attachments of a run (owner DO: WorkDO (owner) + R2). */
export const listRunAttachments: Handler = notImplementedHandler;

/** POST /api/v1/runs/{runId}/attachments: Get an upload URL for a prompt attachment (owner DO: WorkDO (owner) + R2). */
export const createRunAttachment: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/attachments/{name}: Download a prompt attachment (owner DO: WorkDO (owner) + R2). */
export const getRunAttachment: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/inputs: List the message queue of a run (owner DO: WorkDO (owner)). */
export const listRunInputs: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/timeline: Timeline of a run (owner DO: WorkDO (owner)). */
export const getRunTimeline: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/diagnostics: Diagnostics of a run (owner DO: WorkDO (owner)). */
export const getRunDiagnostics: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/sessions: Sessions of a run (owner DO: WorkDO (owner)). */
export const listRunSessions: Handler = notImplementedHandler;

/** GET /api/v1/runs/{runId}/conversation: Normalized conversation of a run (owner DO: WorkDO (owner)). */
export const getRunConversation: Handler = notImplementedHandler;
