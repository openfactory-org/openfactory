// REST handlers: proposals. C0 stubs that answer 501. Owner: WS13.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/definition/proposals: List proposed definition versions (owner DO: TeamDO). */
export const listDefinitionProposals: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/definition/proposals/{id}/approve: Approve and apply a proposed definition version (owner DO: TeamDO). */
export const approveDefinitionProposal: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/definition/proposals/{id}/reject: Reject a proposed definition version (owner DO: TeamDO → WorkDO (message to author run, outbox)). */
export const rejectDefinitionProposal: Handler = notImplementedHandler;
