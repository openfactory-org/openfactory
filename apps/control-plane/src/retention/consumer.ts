// The consumer of Queue messages `retention.sweep`.
import { toOfError } from "@openfactory/core";
import type { JobMessage } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import type { RetentionRpc } from "./sweep";

export type RetentionJob = Extract<JobMessage, { kind: "retention.sweep" }>;

/** The DO binding of a DO name: `team:<t>`, `team:<t>|work:<tok>`, `|index`, `|ingress:`... */
export function bindingOfDoName(
  name: string,
): "TEAM" | "WORK" | "INDEX" | "INGRESS" | "DISPATCH" | "ROUTER" | "BENCHMARK" | undefined {
  const bar = name.indexOf("|");
  if (bar < 0) return name.startsWith("team:") ? "TEAM" : undefined;
  const kind = name.slice(bar + 1).split(":")[0];
  switch (kind) {
    case "work":
      return "WORK";
    case "index":
      return "INDEX";
    case "ingress":
    case "webhook":
      return "INGRESS";
    case "dispatch":
      return "DISPATCH";
    case "router":
      return "ROUTER";
    case "bench":
      return "BENCHMARK";
  }
  return undefined;
}

/**
 * Asks the DO of the message for one sweep step. A step that has more to do
 * sends the next one as a new message. A DO without retentionSweep is skipped.
 */
export async function handleRetentionJob(env: Env, msg: RetentionJob): Promise<void> {
  const binding = bindingOfDoName(msg.doName);
  if (!binding) throw new Error(`retention.sweep: unknown DO name ${JSON.stringify(msg.doName)}`);
  const ns = env[binding] as unknown as DurableObjectNamespace;
  const stub = ns.getByName(msg.doName) as unknown as Partial<RetentionRpc>;
  if (typeof stub.retentionSweep !== "function") return;
  let out: { more: boolean };
  try {
    out = await stub.retentionSweep();
  } catch (e) {
    // A DO that does not serve the method (RPC error) is skipped; anything else is retried by the queue.
    if (toOfError(e).error.code === "feature_not_available") return;
    throw e;
  }
  if (out.more) await env.JOBS.send({ kind: "retention.sweep", msgId: `${msg.msgId}+`, doName: msg.doName });
}
