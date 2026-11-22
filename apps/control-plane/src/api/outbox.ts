// REST handlers: outbox dead letters. Owner: WS5. The dead-letter index is in
// IndexDO; redrive goes to the DO named in the row.
import { doName, notFound, OfError } from "@openfactory/core";
import { callMeta, json, teamOf, type Handler } from "../http/handler";
import type { DO_BINDINGS } from "../contracts/env";

/** GET /api/v1/outbox/dead */
export const listDeadOutboxMessages: Handler = async (_req, env, ctx) =>
  json(
    await env.INDEX.getByName(doName.index(teamOf(ctx))).list(
      callMeta(ctx),
      "deadLetters",
      Object.fromEntries(ctx.url.searchParams),
    ),
  );

/** The DO binding of each kind after the bar of a DO name (doName in @openfactory/core). */
const BINDING_OF_KIND: ReadonlyMap<string, keyof typeof DO_BINDINGS> = new Map([
  ["work", "WORK"],
  ["dispatch", "DISPATCH"],
  ["router", "ROUTER"],
  ["index", "INDEX"],
  ["ingress", "INGRESS"],
  ["worker", "WORKER_HUB"],
  ["bench", "BENCHMARK"],
]);

/** The binding of a DO name ("team:<t>", "team:<t>|work:<x>", ...). */
function bindingOf(name: string): keyof typeof DO_BINDINGS | undefined {
  const bar = name.indexOf("|");
  if (bar < 0) return "TEAM";
  return BINDING_OF_KIND.get(name.slice(bar + 1).split(":")[0] ?? "");
}

/** POST /api/v1/outbox/{id}/redrive */
export const redriveOutboxMessage: Handler = async (_req, env, ctx) => {
  const index = env.INDEX.getByName(doName.index(teamOf(ctx)));
  const dl = await index.findDeadLetter(ctx.params.id ?? "");
  if (!dl) throw notFound(`dead letter ${ctx.params.id}`);
  const binding = bindingOf(dl.originDO);
  if (!binding) throw new OfError("invalid_request", `unknown origin ${dl.originDO}`);
  // IngressDO dead letters are event deliveries; every other row is an outbox row.
  const ok =
    binding === "INGRESS"
      ? await env.INGRESS.getByName(dl.originDO).redrive(dl.msgId)
      : await env[binding].getByName(dl.originDO).outboxRedrive(dl.msgId);
  if (ok) await index.deadLetterRedriven(dl.originDO, dl.msgId);
  return json({ redriven: ok });
};
