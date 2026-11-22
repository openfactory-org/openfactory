// REST handlers: proposals. Owner: WS13. TeamDO keeps the versions; the review
// item belongs to a WorkDO, so the Worker resolves it after the decision.
import { doName } from "@openfactory/core";
import {
  callMeta,
  errorResponse,
  json,
  opParams,
  opResult,
  readJson,
  teamApi,
  teamOf,
  type Handler,
} from "../http/handler";
import { unwrapOp, workEngine } from "../scoring/clients";

/** GET /api/v1/factories/{uid}/definition/proposals. */
export const listDefinitionProposals: Handler = teamApi();

async function decide(
  op: string,
  request: Request,
  env: Parameters<Handler>[1],
  ctx: Parameters<Handler>[2],
): Promise<Response> {
  try {
    const body = op === "rejectDefinitionProposal" ? await readJson(request) : undefined;
    const teamId = teamOf(ctx);
    const stub = env.TEAM.getByName(doName.team(teamId));
    const out = unwrapOp<
      Record<string, unknown> & {
        $resolve?: {
          inbox_item_id: string;
          resolution: string;
          actor: string;
          author_run_id?: string;
          proposal_id?: string;
        };
      }
    >(await stub.api(callMeta(ctx), op, opParams(ctx), body));
    const { $resolve, ...view } = out;
    if ($resolve?.inbox_item_id) {
      const engine = workEngine(env, teamId);
      try {
        // The engine sends the resolution to the author run.
        await engine.resolveInbox($resolve.inbox_item_id, $resolve.resolution, $resolve.actor);
      } catch (e) {
        // Resolved already: a rejection still reaches the run as a message.
        if ($resolve.author_run_id) {
          await engine
            .followUp(
              $resolve.author_run_id,
              `[human response] Your proposed definition change ${$resolve.proposal_id ?? ""} was ${$resolve.resolution}`,
              $resolve.actor,
            )
            .catch(() => undefined);
        } else if (!(e instanceof Error)) throw e;
      }
    }
    return json(view);
  } catch (e) {
    return errorResponse(e, { instance: ctx.url.pathname, requestId: ctx.requestId });
  }
}

/** POST .../proposals/{id}/approve: applies the version; 409 when the base is not the active version. */
export const approveDefinitionProposal: Handler = (req, env, ctx) => decide("approveDefinitionProposal", req, env, ctx);

/** POST .../proposals/{id}/reject: the comment goes to the author run. */
export const rejectDefinitionProposal: Handler = (req, env, ctx) => decide("rejectDefinitionProposal", req, env, ctx);

export { opResult };
