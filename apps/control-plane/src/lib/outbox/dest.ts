// Outbox destinations. A destination is a string so it can sit in the row.
//
//   do:<BINDING>:<name>   a DO that implements OutboxReceiver.receive (BINDING is a key of DO_BINDINGS)
//   queue:JOBS            the JOBS queue (payload is the JobMessage)
//   <other>:<...>         custom; the DO handles it by overriding BaseDO.deliverCustom
import { doName } from "@openfactory/core";
import type { Ack, JobMessage, OutboxEnvelope, OutboxReceiver } from "../../contracts/do-messages";
import { DO_BINDINGS, type Env } from "../../contracts/env";

export type DoBinding = keyof typeof DO_BINDINGS;

export const dest = {
  do: (binding: DoBinding, name: string): string => `do:${binding}:${name}`,
  team: (teamId: string): string => `do:TEAM:${doName.team(teamId)}`,
  index: (teamId: string): string => `do:INDEX:${doName.index(teamId)}`,
  dispatch: (teamId: string): string => `do:DISPATCH:${doName.dispatch(teamId)}`,
  router: (teamId: string): string => `do:ROUTER:${doName.router(teamId)}`,
  work: (teamId: string, token: string): string => `do:WORK:${doName.work(teamId, token)}`,
  ingress: (teamId: string, partition: string): string => `do:INGRESS:${doName.ingress(teamId, partition)}`,
  workerHub: (teamId: string, workerId: string): string => `do:WORKER_HUB:${doName.workerHub(teamId, workerId)}`,
  benchmark: (teamId: string, token: string): string => `do:BENCHMARK:${doName.benchmark(teamId, token)}`,
  jobs: (): string => "queue:JOBS",
} as const;

export type ParsedDest =
  | { scheme: "do"; binding: DoBinding; name: string }
  | { scheme: "queue"; queue: "JOBS" }
  | { scheme: "custom"; raw: string };

export function parseDest(d: string): ParsedDest {
  if (d.startsWith("do:")) {
    const rest = d.slice(3);
    const i = rest.indexOf(":");
    const binding = rest.slice(0, i);
    if (i > 0 && binding in DO_BINDINGS)
      return { scheme: "do", binding: binding as DoBinding, name: rest.slice(i + 1) };
  }
  if (d === "queue:JOBS") return { scheme: "queue", queue: "JOBS" };
  return { scheme: "custom", raw: d };
}

/** Delivers to a DO or the queue. Returns undefined for a custom destination. */
export async function deliverStandard(env: Env, d: string, envelope: OutboxEnvelope): Promise<Ack | undefined> {
  const p = parseDest(d);
  if (p.scheme === "do") {
    const ns = env[p.binding] as unknown as DurableObjectNamespace;
    const stub = ns.getByName(p.name) as unknown as OutboxReceiver;
    return await stub.receive(envelope);
  }
  if (p.scheme === "queue") {
    await env.JOBS.send(envelope.payload as unknown as JobMessage);
    return { ok: true };
  }
  return undefined;
}

/** Team ID of a DO name "team:<id>|...", or "" . */
export function teamOfDoName(name: string): string {
  if (!name.startsWith("team:")) return "";
  const rest = name.slice(5);
  const i = rest.indexOf("|");
  return i < 0 ? rest : rest.slice(0, i);
}
