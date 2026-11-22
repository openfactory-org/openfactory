// REST handlers: workers. C0 stubs that answer 501. Owner: WS6.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/workers: List workers (owner DO: TeamDO (registry) + WorkerHubDO (live)). */
export const listWorkers: Handler = notImplementedHandler;

/** POST /api/v1/workers/enrollments: Create a worker enrollment token (owner DO: TeamDO (registry) + WorkerHubDO). */
export const createWorkerEnrollment: Handler = notImplementedHandler;

/** POST /api/v1/workers/enroll: Enroll a worker host (owner DO: TeamDO (registry) + WorkerHubDO). */
export const enrollWorker: Handler = notImplementedHandler;

/** GET /api/v1/workers/{id}: Get a worker (owner DO: TeamDO (registry) + WorkerHubDO (live)). */
export const getWorker: Handler = notImplementedHandler;

/** PATCH /api/v1/workers/{id}: Update a worker (owner DO: TeamDO (registry) + WorkerHubDO). */
export const updateWorker: Handler = notImplementedHandler;

/** DELETE /api/v1/workers/{id}: Delete a worker (owner DO: TeamDO (registry) + WorkerHubDO). */
export const deleteWorker: Handler = notImplementedHandler;

/** POST /api/v1/workers/{id}/drain: Drain a worker (owner DO: TeamDO (registry) + WorkerHubDO). */
export const drainWorker: Handler = notImplementedHandler;

/** POST /api/v1/workers/{id}/resume: Resume a worker (owner DO: TeamDO (registry) + WorkerHubDO). */
export const resumeWorker: Handler = notImplementedHandler;

/** POST /api/v1/workers/{id}/revoke: Revoke a worker (owner DO: TeamDO (registry) + WorkerHubDO). */
export const revokeWorker: Handler = notImplementedHandler;

/** GET /api/v1/workers/{id}/git-credential: Get a git credential for the worker cache (owner DO: TeamDO (registry) + WorkerHubDO). */
export const getWorkerGitCredential: Handler = notImplementedHandler;
