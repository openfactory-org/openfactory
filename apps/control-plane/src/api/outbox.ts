// REST handlers: outbox dead letters. Owner: WS5. The dead-letter index is in
// IndexDO; redrive goes to the DO named in the row.
import { doName, notFound, OfError } from "@openfactory/core";
import type { Handler } from "../http/handler";
import type { DO_BINDINGS } from "../contracts/env";
import { type Env } from "../contracts/env";
import { json, metaOf, teamOf } from "./runs";

/** GET /api/v1/outbox/dead */
export const listDeadOutboxMessages: Handler = async (_req, env, ctx) =>
  json(
    200,
    await env.INDEX.getByName(doName.index(teamOf(ctx))).list(
      metaOf(ctx),
      "deadLetters",
      Object.fromEntries(ctx.url.searchParams),
    ),
  );

/** The namespace of a DO name ("team:<t>", "team:<t>|work:<x>", ...). */
function namespaceOf(env: Env, name: string): DurableObjectNamespace | undefined {
  const rest = name.includes("|") ? name.slice(name.indexOf("|") + 1) : "";
  const kind = rest.split(":")[0] ?? "";
  const binding: keyof typeof DO_BINDINGS | undefined =
    rest === ""
      ? "TEAM"
      : kind === "work"
        ? "WORK"
        : kind === "dispatch"
          ? "DISPATCH"
          : kind === "router"
            ? "ROUTER"
            : kind === "index"
              ? "INDEX"
              : kind === "ingress"
                ? "INGRESS"
                : kind === "worker"
                  ? "WORKER_HUB"
                  : kind === "bench"
                    ? "BENCHMARK"
                    : undefined;
  return binding ? (env[binding] as unknown as DurableObjectNamespace) : undefined;
}

/** POST /api/v1/outbox/{id}/redrive */
export const redriveOutboxMessage: Handler = async (_req, env, ctx) => {
  const index = env.INDEX.getByName(doName.index(teamOf(ctx)));
  const dl = await index.findDeadLetter(ctx.params.id ?? "");
  if (!dl) throw notFound(`dead letter ${ctx.params.id}`);
  const ns = namespaceOf(env, dl.originDO);
  if (!ns) throw new OfError("invalid_request", `unknown origin ${dl.originDO}`);
  const stub = ns.getByName(dl.originDO) as unknown as {
    outboxRedrive?: (id: string) => Promise<boolean>;
    redrive?: (id: string) => Promise<boolean>;
  };
  const ok =
    stub.redrive && dl.originDO.includes("|ingress:")
      ? await stub.redrive(dl.msgId)
      : await stub.outboxRedrive!(dl.msgId);
  if (ok) await index.deadLetterRedriven(dl.originDO, dl.msgId);
  return json(200, { redriven: ok });
};
