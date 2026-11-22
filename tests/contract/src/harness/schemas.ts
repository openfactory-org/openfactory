// JSON Schema validation for the contract suite: REST responses against
// api/openapi.json, runner frames against schemas/runner/v1, worker frames
// against schemas/worker/v2.
import { readFileSync } from "node:fs";
import { Validator, type OutputUnit, type Schema } from "@cfworker/json-schema";
import { repoPath } from "./paths.ts";

type Json = Record<string, unknown>;

function readJson(rel: string): Json {
  return JSON.parse(readFileSync(repoPath(rel), "utf8")) as Json;
}

export const openapi = readJson("api/openapi.json") as {
  paths: Record<string, Record<string, OpenApiOperation>>;
  components: Json & { schemas: Record<string, Json> };
};

export interface OpenApiOperation {
  operationId?: string;
  tags?: string[];
  responses?: Record<string, { $ref?: string; content?: Record<string, { schema?: Json }> }>;
  "x-required-role"?: string;
}

/** A schema check failed. The message lists the first errors. */
export class SchemaError extends Error {
  constructor(
    what: string,
    public readonly errors: OutputUnit[],
    value: unknown,
  ) {
    const leaves = errors.filter((e) => !/subschema|does not match|had errors/i.test(e.error));
    const shown = (leaves.length > 0 ? leaves : errors).slice(0, 6);
    super(
      `${what} does not match its schema:\n` +
        shown.map((e) => `  ${e.instanceLocation}: ${e.error} (${e.keywordLocation})`).join("\n") +
        `\nvalue: ${JSON.stringify(value).slice(0, 600)}`,
    );
    this.name = "SchemaError";
  }
}

const validators = new Map<string, Validator>();

function cached(key: string, make: () => Validator): Validator {
  let v = validators.get(key);
  if (!v) {
    v = make();
    validators.set(key, v);
  }
  return v;
}

function check(v: Validator, what: string, value: unknown): void {
  const r = v.validate(value);
  if (!r.valid) throw new SchemaError(what, r.errors, value);
}

// ---------------------------------------------------------------- OpenAPI

interface PathEntry {
  template: string;
  re: RegExp;
  params: number;
}

const pathEntries: PathEntry[] = Object.keys(openapi.paths)
  .map((template) => {
    const parts = template.split("/").map((seg) => (/^\{.+\}$/.test(seg) ? "[^/]+" : escapeRe(seg)));
    return { template, re: new RegExp(`^${parts.join("/")}$`), params: (template.match(/\{/g) ?? []).length };
  })
  // Literal paths win over templated ones (/workers/enroll before /workers/{id}).
  .sort((a, b) => a.params - b.params);

function escapeRe(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** The openapi path template of a concrete API path (without /api/v1), or undefined. */
export function matchPath(path: string): string | undefined {
  const clean = path.split("?")[0] ?? path;
  return pathEntries.find((e) => e.re.test(clean))?.template;
}

export function operationFor(method: string, path: string): { template: string; op: OpenApiOperation } | undefined {
  const template = matchPath(path);
  if (!template) return undefined;
  const op = openapi.paths[template]?.[method.toLowerCase()];
  return op ? { template, op } : undefined;
}

function resolveResponse(ref: string): { content?: Record<string, { schema?: Json }> } {
  const name = ref.replace("#/components/responses/", "");
  const responses = openapi.components.responses as Record<string, { content?: Record<string, { schema?: Json }> }>;
  const r = responses[name];
  if (!r) throw new Error(`openapi: unknown response ${ref}`);
  return r;
}

/** The JSON schema of a response, or undefined when the spec declares no JSON body. */
export function responseSchema(method: string, path: string, status: number): Json | undefined {
  const found = operationFor(method, path);
  if (!found) return undefined;
  const responses = found.op.responses ?? {};
  let resp = responses[String(status)] ?? responses[`${String(status)[0]}XX`] ?? responses.default;
  if (!resp) return undefined;
  if (resp.$ref) resp = resolveResponse(resp.$ref);
  const content = resp.content ?? {};
  const media = content["application/json"] ?? content["application/problem+json"];
  return media?.schema;
}

/** Validates a JSON response body against the openapi schema of its operation and status. */
export function assertOpenApiResponse(method: string, path: string, status: number, body: unknown): void {
  const schema = responseSchema(method, path, status);
  if (!schema) return;
  const found = operationFor(method, path);
  const key = `openapi:${method}:${found?.template}:${status}`;
  const v = cached(key, () => new Validator({ ...schema, components: openapi.components }, "2020-12", false));
  const what = `${method.toUpperCase()} ${found?.template ?? path}`;
  check(v, `${what} ${status} response`, body);
}

/** Validates a value against a component schema of api/openapi.json. */
export function assertComponent(name: string, value: unknown): void {
  const v = cached(
    `component:${name}`,
    () => new Validator({ $ref: `#/components/schemas/${name}`, components: openapi.components }, "2020-12", false),
  );
  check(v, `components/schemas/${name}`, value);
}

// ---------------------------------------------------------------- runner/v1

export type RunnerSchemaFile = "session" | "events" | "commands" | "transfer" | "conversation";

const runnerDocs = new Map<RunnerSchemaFile, Json>();

function runnerDoc(file: RunnerSchemaFile): Json {
  let d = runnerDocs.get(file);
  if (!d) {
    d = readJson(`schemas/runner/v1/${file}.json`);
    runnerDocs.set(file, d);
  }
  return d;
}

/** Validates a value against `$defs/<def>` of schemas/runner/v1/<file>.json. */
export function assertRunner(file: RunnerSchemaFile, def: string, value: unknown): void {
  const v = cached(`runner:${file}:${def}`, () => {
    const doc = runnerDoc(file);
    const defs = doc.$defs as Record<string, Json> | undefined;
    if (!defs?.[def]) throw new Error(`schemas/runner/v1/${file}.json has no $defs/${def}`);
    return new Validator({ $defs: defs, $ref: `#/$defs/${def}` }, "2020-12", false);
  });
  check(v, `runner/v1 ${file}#${def}`, value);
}

// ---------------------------------------------------------------- worker/v2

let workerDoc: Json | undefined;

function workerProtocol(): Json {
  workerDoc ??= readJson("schemas/worker/v2/protocol.json");
  return workerDoc;
}

/** Validates one worker protocol frame (the oneOf of schemas/worker/v2/protocol.json). */
export function assertWorkerFrame(frame: unknown): void {
  const v = cached("worker:frame", () => {
    const doc = workerProtocol();
    return new Validator({ $defs: doc.$defs, oneOf: doc.oneOf } as Schema, "2020-12", false);
  });
  check(v, `worker/v2 frame`, frame);
}

/** Validates a value against `$defs/<def>` of schemas/worker/v2/protocol.json. */
export function assertWorkerDef(def: string, value: unknown): void {
  const v = cached(`worker:${def}`, () => {
    const doc = workerProtocol();
    return new Validator({ $defs: doc.$defs, $ref: `#/$defs/${def}` }, "2020-12", false);
  });
  check(v, `worker/v2 #${def}`, value);
}
