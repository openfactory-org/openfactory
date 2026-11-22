// The API client of one command run, plus the helpers that several commands share.

import { ApiError, isInvalid, isNotFound, OpenFactoryClient, unwrap, type components } from "@openfactory/api-client";
import { CliError, type Env, type Globals } from "./framework.ts";
import { VERSION } from "./version.ts";

/** The REST API schemas (api/openapi.json). */
export type Schemas = components["schemas"];
export type Factory = Schemas["Factory"];

/** The values of the global --output flag. */
const OUTPUT_FORMATS = ["text", "json"];

/** Creates the client. The API key is required unless allowAnonymous is set. */
export function makeClient(g: Globals, env: Env, allowAnonymous = false): OpenFactoryClient {
  if (!OUTPUT_FORMATS.includes(g.output))
    throw new CliError(`invalid --output ${JSON.stringify(g.output)} (want text or json)`);
  if (g.apiKey === "" && !allowAnonymous) throw new CliError("no API key: pass --api-key or set OF_API_KEY");
  const opts: ConstructorParameters<typeof OpenFactoryClient>[0] = {
    baseUrl: g.server.replace(/\/+$/, ""),
    userAgent: `openfactory/${VERSION}`,
    fetch: env.fetch,
  };
  if (g.apiKey !== "") opts.apiKey = g.apiKey;
  return new OpenFactoryClient(opts);
}

export const enc = encodeURIComponent;

/** Accepts a factory UID, alias or name. */
export async function resolveFactory(c: OpenFactoryClient, ref: string): Promise<Factory> {
  try {
    return await unwrap(c.api.GET("/factories/{uid}", { params: { path: { uid: ref } } }));
  } catch (err) {
    if (!isNotFound(err) && !isInvalid(err)) throw err;
  }
  const all = await c.list<Factory>("factories").collect();
  const alias = ref.replace(/^@/, "").toLowerCase();
  const name = ref.toLowerCase();
  const hit = all.find((f) => f.alias.toLowerCase() === alias || f.name.toLowerCase() === name);
  if (!hit) throw new CliError(`factory ${JSON.stringify(ref)} not found`);
  return hit;
}

/**
 * Expands the validation errors of an API answer into the message. App.execute
 * applies it to every error that a command throws.
 */
export function explain(err: unknown): unknown {
  if (err instanceof ApiError && err.errors.length > 0) {
    let msg = err.message;
    for (const v of err.errors) {
      let loc = v.file ?? "";
      if (v.line && v.line > 0) loc += ":" + String(v.line);
      msg += `\n  ${loc}: ${v.message}`;
    }
    return new CliError(msg);
  }
  return err;
}

export const isJSON = (g: Globals): boolean => g.output === "json";
