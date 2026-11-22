// REST handlers: schedules and manual automation runs (owner WS10a).
import { OfError, doName, type Automation, type Event } from "@openfactory/core";
import type { Principal } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { callMeta, json, opParams, opResult, teamApi, teamOf, type Handler } from "../http/handler";
import { manualEvent } from "../triggers/schedules";

/** GET /api/v1/factories/{uid}/schedules */
export const listSchedules: Handler = teamApi();
/** POST /api/v1/factories/{uid}/schedules/{id}/pause */
export const pauseSchedule: Handler = teamApi();
/** POST /api/v1/factories/{uid}/schedules/{id}/resume */
export const resumeSchedule: Handler = teamApi();

/**
 * Starts the automation's run for a manual event and returns the run (201).
 * Registered by the engine (WS5). Without it the event goes to IngressDO and
 * the call answers 202.
 */
export type ManualRunDispatcher = (
  env: Env,
  input: { teamId: string; factoryId: string; automation: Automation; event: Event; principal: Principal },
) => Promise<unknown>;

let dispatcher: ManualRunDispatcher | undefined;

export function setManualRunDispatcher(fn: ManualRunDispatcher): void {
  dispatcher = fn;
}

/** POST /api/v1/automations/{id}/run (TeamDO → IngressDO → WorkDO). */
export const runAutomation: Handler = async (_request, env, ctx) => {
  const team = env.TEAM.getByName(doName.team(teamOf(ctx)));
  const res = opResult(await team.api(callMeta(ctx), "automationForRun", opParams(ctx), undefined));
  const out = (await res.json()) as {
    automation: Automation;
    factory: { id: string };
    team_id: string;
    principal: Principal;
  };
  const p = out.principal;
  const actor = {
    provider: "factory",
    external_id: "",
    handle: p.userId ?? p.id,
    is_bot: false,
    ...(p.userId ? { user_id: p.userId } : {}),
  };
  const ev = manualEvent(out.automation, out.team_id, actor, Date.now());
  if (dispatcher) {
    const run = await dispatcher(env, {
      teamId: out.team_id,
      factoryId: out.factory.id,
      automation: out.automation,
      event: ev,
      principal: p,
    });
    if (!run) throw new OfError("internal_error", "engine returned no run");
    return json(run, 201);
  }
  const partition = "manual:" + out.automation.id;
  await env.INGRESS.getByName(doName.ingress(out.team_id, partition)).enqueue(partition, ev as never, ev.id);
  return json({ event_id: ev.id, automation_id: out.automation.id, status: "accepted" }, 202);
};
