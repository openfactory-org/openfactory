// Request body helpers for ops and byte-capped reads of request and response
// bodies. A field of the wrong JSON type is a 400 (invalid_request) that names
// the field and the JSON type it needs; null is the same as an absent field.
import { OfError } from "@openfactory/core";

export type JsonObject = Record<string, unknown>;

/** The body as an object; {} for an absent body when optional. */
export function bodyObject(body: unknown, optional = false): JsonObject {
  if (body === undefined || body === null) {
    if (optional) return {};
    throw new OfError("invalid_request", "request body is required");
  }
  if (typeof body !== "object" || Array.isArray(body))
    throw new OfError("invalid_request", "malformed JSON body: expected an object");
  return body as JsonObject;
}

/** want: the JSON type in plain words ("a string", "an array of strings"). */
function typeErr(field: string, want: string): OfError {
  return new OfError("invalid_request", `field "${field}" must be ${want}`);
}

export function optString(o: JsonObject, k: string): string | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "string") throw typeErr(k, "a string");
  return v;
}

export function str(o: JsonObject, k: string): string {
  return optString(o, k) ?? "";
}

export function optBool(o: JsonObject, k: string): boolean | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "boolean") throw typeErr(k, "a boolean");
  return v;
}

export function optStringArray(o: JsonObject, k: string): string[] | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (!Array.isArray(v) || !v.every((x) => typeof x === "string")) throw typeErr(k, "an array of strings");
  return v as string[];
}

export function optStringMap(o: JsonObject, k: string): Record<string, string> | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "object" || Array.isArray(v) || !Object.values(v).every((x) => typeof x === "string")) {
    throw typeErr(k, "an object with string values");
  }
  return v as Record<string, string>;
}

/** A nested object; undefined when absent. */
export function optObject(o: JsonObject, k: string): JsonObject | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "object" || Array.isArray(v)) throw typeErr(k, "an object");
  return v as JsonObject;
}

/** Map of string or null values (for "null removes"). */
export function optNullableStringMap(o: JsonObject, k: string): Record<string, string | null> | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (
    typeof v !== "object" ||
    Array.isArray(v) ||
    !Object.values(v).every((x) => x === null || typeof x === "string")
  ) {
    throw typeErr(k, "an object with string or null values");
  }
  return v as Record<string, string | null>;
}

export function now(): string {
  return new Date().toISOString();
}

// The byte-capped body readers live in @openfactory/core; they are re-exported
// here for the callers that import them from this module.
export { readBytesCapped, readTextCapped, readTextPrefix } from "@openfactory/core";
