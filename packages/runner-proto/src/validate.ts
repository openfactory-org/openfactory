// Runtime validators for runner protocol v1 messages, from the JSON Schemas in
// schemas/runner/v1. @cfworker/json-schema works in Workers (no eval).

import { Validator, type OutputUnit, type Schema } from "@cfworker/json-schema";
import commands from "../../../schemas/runner/v1/commands.json";
import conversation from "../../../schemas/runner/v1/conversation.json";
import events from "../../../schemas/runner/v1/events.json";
import session from "../../../schemas/runner/v1/session.json";
import transfer from "../../../schemas/runner/v1/transfer.json";
import type {
  CommandsDefinitions,
  ConversationDefinitions,
  EventsDefinitions,
  SessionDefinitions,
  TransferDefinitions,
} from "./generated/index";

/** The schema documents of runner protocol v1, by file name. */
export const RUNNER_SCHEMAS = { commands, conversation, events, session, transfer } as const;

interface RunnerSchemaDefs {
  commands: CommandsDefinitions;
  conversation: ConversationDefinitions;
  events: EventsDefinitions;
  session: SessionDefinitions;
  transfer: TransferDefinitions;
}

/** "<schema>#<definition>", for example "events#EventBatch". */
export type RunnerDefName = {
  [S in keyof RunnerSchemaDefs]: `${S}#${keyof RunnerSchemaDefs[S] & string}`;
}[keyof RunnerSchemaDefs];

/** The generated type of a definition name. */
export type RunnerDefType<N extends RunnerDefName> = N extends `${infer S}#${infer D}`
  ? S extends keyof RunnerSchemaDefs
    ? D extends keyof RunnerSchemaDefs[S]
      ? NonNullable<RunnerSchemaDefs[S][D]>
      : never
    : never
  : never;

export interface SchemaError {
  /** JSON pointer of the instance location, for example "#/events/0/seq". */
  instanceLocation: string;
  /** The schema keyword that failed. */
  keyword: string;
  /** The schema location. */
  keywordLocation: string;
  error: string;
}

export type ValidationResult<T> = { valid: true; value: T; errors: [] } | { valid: false; errors: SchemaError[] };

const validators = new Map<string, Validator>();

function schemaId(name: keyof typeof RUNNER_SCHEMAS): string {
  return (RUNNER_SCHEMAS[name] as { $id: string }).$id;
}

function validatorFor(def: string): Validator {
  let v = validators.get(def);
  if (v) return v;
  const [file, name] = def.split("#") as [keyof typeof RUNNER_SCHEMAS, string];
  const doc = RUNNER_SCHEMAS[file] as { $defs?: Record<string, unknown> } | undefined;
  if (!doc || !doc.$defs || !(name in doc.$defs)) throw new Error(`runner-proto: unknown definition ${def}`);
  v = new Validator({ $ref: `${schemaId(file)}#/$defs/${name}` }, "2020-12", false);
  for (const k of Object.keys(RUNNER_SCHEMAS) as (keyof typeof RUNNER_SCHEMAS)[]) {
    v.addSchema(structuredClone(RUNNER_SCHEMAS[k]) as Schema, schemaId(k));
  }
  validators.set(def, v);
  return v;
}

/** Converts validator output units into schema errors (leaf errors only). */
export function toSchemaErrors(units: readonly OutputUnit[]): SchemaError[] {
  return units.map((u) => ({
    instanceLocation: u.instanceLocation,
    keyword: u.keyword,
    keywordLocation: u.keywordLocation,
    error: u.error,
  }));
}

/** Validates a value against a runner protocol definition. */
export function validateRunner<N extends RunnerDefName>(def: N, value: unknown): ValidationResult<RunnerDefType<N>> {
  const r = validatorFor(def).validate(value);
  if (r.valid) return { valid: true, value: value as RunnerDefType<N>, errors: [] };
  return { valid: false, errors: toSchemaErrors(r.errors) };
}

/** Reports whether a value matches a runner protocol definition. */
export function isRunner<N extends RunnerDefName>(def: N, value: unknown): value is RunnerDefType<N> {
  return validatorFor(def).validate(value).valid;
}

/** Validates and returns the value, or throws an Error that lists the problems. */
export function parseRunner<N extends RunnerDefName>(def: N, value: unknown): RunnerDefType<N> {
  const r = validateRunner(def, value);
  if (!r.valid) {
    const first = r.errors
      .slice(0, 5)
      .map((e) => `${e.instanceLocation} ${e.keyword}: ${e.error}`)
      .join("; ");
    throw new Error(`runner-proto: invalid ${def}: ${first}`);
  }
  return r.value;
}
