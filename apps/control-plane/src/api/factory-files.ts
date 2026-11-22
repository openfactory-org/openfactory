// REST handlers: factory-file JSON Schemas (public, WS4). Port of
// handleSchemaVersions, handleSchemaVersion and handleSchemaDocument.
import { OfError } from "@openfactory/core";
import { schemaVersions, schemas } from "@openfactory/definition";
import { json, type Handler } from "../http/handler";

const CACHE = { "cache-control": "public, max-age=300" };

function documents(version: string): Record<string, string> {
  if (version !== schemaVersions().current) throw new OfError("resource_not_found", `schema version ${version} not found`);
  return schemas();
}

/** GET /factory-files/schemas */
export const listSchemaVersions: Handler = async () => {
  const v = schemaVersions();
  return json({ current_version: v.current, versions: v.versions });
};

/** GET /factory-files/schemas/{version}: every document, keyed by file name. */
export const getSchemaVersion: Handler = async (_r, _e, ctx) => {
  const docs = documents(ctx.params["version"]!);
  const body = "{" + Object.entries(docs).map(([k, v]) => `${JSON.stringify(k)}:${v.trim()}`).join(",") + "}\n";
  return new Response(body, { headers: { "content-type": "application/json", ...CACHE } });
};

/** GET /factory-files/schemas/{version}/{document}: ".schema.json" suffix optional. */
export const getSchemaDocument: Handler = async (_r, _e, ctx) => {
  const docs = documents(ctx.params["version"]!);
  const d = ctx.params["document"]!;
  for (const cand of [d, d + ".schema.json", d + ".json"]) {
    const doc = docs[cand];
    if (doc !== undefined) return new Response(doc, { headers: { "content-type": "application/schema+json", ...CACHE } });
  }
  throw new OfError("resource_not_found", `schema document ${d} not found`);
};
