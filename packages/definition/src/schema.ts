// Schema catalog. Port of the catalog parts of internal/definition/schema.go.
// The documents come from the generator (schemagen.ts); they equal the files
// in schemas/definition/v1/ (checked by the tests and gen-schemas --check).

import { normalizePath } from "./compile.ts";
import { SCHEMA_VERSION } from "./common.ts";
import { FACTORY_FILE } from "./factory.ts";
import { generatedSchemas, goJSONIndent, SCHEMA_NAMES } from "./schemagen.ts";

export { SCHEMA_BASE_URL, SCHEMA_NAMES } from "./schemagen.ts";

export interface SchemaCatalog {
  current: string;
  versions: string[];
}

/** schemaVersions returns the supported schema versions and the current one. */
export function schemaVersions(): SchemaCatalog {
  return { current: SCHEMA_VERSION, versions: [SCHEMA_VERSION] };
}

let cache: Record<string, string> | null = null;

function texts(): Record<string, string> {
  if (cache === null) {
    const g = generatedSchemas();
    cache = {};
    for (const n of SCHEMA_NAMES) cache[n] = goJSONIndent(g[n]);
  }
  return cache;
}

/** schema returns one schema document as JSON text. name may be "factory", "factory.schema.json" or "v1/factory.schema.json". */
export function schema(name: string): string | null {
  let n = name;
  if (n.startsWith(SCHEMA_VERSION + "/")) n = n.slice(SCHEMA_VERSION.length + 1);
  if (n.endsWith(".json")) n = n.slice(0, -5);
  if (n.endsWith(".schema")) n = n.slice(0, -7);
  return texts()[n] ?? null;
}

/** schemas returns every schema document keyed by file name (for example "factory.schema.json"). */
export function schemas(): Record<string, string> {
  const out: Record<string, string> = {};
  for (const n of SCHEMA_NAMES) out[n + ".schema.json"] = texts()[n]!;
  return out;
}

/** schemaForPath returns the schema document name for a definition path, or "". */
export function schemaForPath(path: string): string {
  const p = normalizePath(path);
  const seg = p.split("/");
  if (p === FACTORY_FILE) return "factory";
  if (seg.length === 3 && seg[0] === "agents" && seg[2] === "agent.md") return "agent";
  if (seg.length === 3 && seg[0] === "automations" && seg[2] === "automation.md") return "automation";
  if (seg.length === 3 && seg[0] === "scorers" && seg[2] === "scorer.md") return "scorer";
  if (seg.length === 2 && seg[0] === "machines" && p.endsWith(".yaml")) return "machine";
  if (seg.length === 2 && seg[0] === "webhooks" && p.endsWith(".yaml")) return "webhook";
  if (seg.length === 3 && seg[0] === "benchmarks" && seg[2] === "suite.yaml") return "benchmark_suite";
  if (seg.length === 4 && seg[0] === "benchmarks" && seg[2] === "tasks" && p.endsWith(".yaml"))
    return "benchmark_suite_task";
  return "";
}
