// Runtime validators for worker protocol v2 frames, from
// schemas/worker/v2/protocol.json. @cfworker/json-schema works in Workers.

import { Validator, type OutputUnit, type Schema } from "@cfworker/json-schema";
import protocol from "../../../schemas/worker/v2/protocol.json";
import type { OpenFactoryWorkerProtocolV2Frame } from "./generated/index";

/** The schema document of worker protocol v2. */
export const WORKER_SCHEMA = protocol;

export type WorkerFrame = OpenFactoryWorkerProtocolV2Frame;

/** A definition name of the schema ($defs), for example "task" or "hello". */
export type WorkerDefName = keyof (typeof protocol)["$defs"] & string;

export interface SchemaError {
  /** JSON pointer of the instance location, for example "#/params/task/env". */
  instanceLocation: string;
  keyword: string;
  keywordLocation: string;
  error: string;
}

export type ValidationResult<T> = { valid: true; value: T; errors: [] } | { valid: false; errors: SchemaError[] };

const SCHEMA_ID = (protocol as { $id: string }).$id;
const validators = new Map<string, Validator>();

function validatorFor(def: string | null): Validator {
  const key = def ?? "";
  let v = validators.get(key);
  if (v) return v;
  if (def !== null && !((protocol.$defs as Record<string, unknown>)[def] !== undefined)) {
    throw new Error(`worker-proto: unknown definition ${def}`);
  }
  v = new Validator({ $ref: def === null ? SCHEMA_ID : `${SCHEMA_ID}#/$defs/${def}` }, "2020-12", false);
  v.addSchema(structuredClone(protocol) as Schema, SCHEMA_ID);
  validators.set(key, v);
  return v;
}

function toErrors(units: readonly OutputUnit[]): SchemaError[] {
  return units.map((u) => ({
    instanceLocation: u.instanceLocation,
    keyword: u.keyword,
    keywordLocation: u.keywordLocation,
    error: u.error,
  }));
}

/** Validates one frame (any frame type). */
export function validateFrame(value: unknown): ValidationResult<WorkerFrame> {
  const r = validatorFor(null).validate(value);
  if (r.valid) return { valid: true, value: value as WorkerFrame, errors: [] };
  return { valid: false, errors: toErrors(r.errors) };
}

/** Validates a value against one definition of the schema. */
export function validateWorkerDef<T = unknown>(def: WorkerDefName, value: unknown): ValidationResult<T> {
  const r = validatorFor(def).validate(value);
  if (r.valid) return { valid: true, value: value as T, errors: [] };
  return { valid: false, errors: toErrors(r.errors) };
}

/** Reports whether a value is a valid frame. */
export function isFrame(value: unknown): value is WorkerFrame {
  return validatorFor(null).validate(value).valid;
}
