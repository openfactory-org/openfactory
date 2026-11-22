// Worker-side calls of the TeamDO mcp module, and the injectable runtime of
// the MCP code (fetch, clock, encrypter), so that tests replace the network.
import { DEFAULT_TEAM, OfError, doName, type ErrorCode } from "@openfactory/core";
import type { CallMeta } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { localEncrypter, type Encrypter } from "./envelope";

/** What the MCP Worker code needs besides env. Tests pass fakes. */
export interface McpRuntime {
  fetch: typeof fetch;
  now: () => number;
  encrypter: () => Promise<Encrypter>;
}

export function defaultRuntime(env: Env): McpRuntime {
  let enc: Promise<Encrypter> | undefined;
  return {
    fetch: (input, init) => fetch(input, init),
    now: () => Date.now(),
    encrypter: () => (enc ??= localEncrypter(env)),
  };
}

/** TeamDO stub of a team (the core has one team: team:default). */
export function teamStub(env: Env, teamId = DEFAULT_TEAM) {
  return env.TEAM.getByName(doName.team(teamId || DEFAULT_TEAM));
}

/** Calls TeamDO.api and unwraps its result: an OpError becomes a thrown OfError, an OpResponse its body. */
export async function teamOp<T = unknown>(
  env: Env,
  op: string,
  body: unknown,
  opts: { meta?: CallMeta; params?: Record<string, string>; teamId?: string } = {},
): Promise<T> {
  const out = (await teamStub(env, opts.teamId).api(
    opts.meta ?? { requestId: "mcp" },
    op,
    opts.params ?? {},
    body,
  )) as unknown;
  if (typeof out === "object" && out !== null) {
    const e = (out as { $error?: { code: string; detail: string; extra?: Record<string, unknown> } }).$error;
    if (e) throw new OfError(e.code as ErrorCode, e.detail, e.extra ? { extra: e.extra } : {});
    if (typeof (out as { $status?: unknown }).$status === "number") return (out as { body?: T }).body as T;
  }
  return out as T;
}
