// Request body helpers for ops (port of decodeJSON's type errors) and
// byte-capped reads of request and response bodies.
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

function typeErr(field: string, type: string): OfError {
  return new OfError("invalid_request", `field "${field}" must be of type ${type}`);
}

export function optString(o: JsonObject, k: string): string | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "string") throw typeErr(k, "string");
  return v;
}

export function str(o: JsonObject, k: string): string {
  return optString(o, k) ?? "";
}

export function optBool(o: JsonObject, k: string): boolean | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "boolean") throw typeErr(k, "bool");
  return v;
}

export function optStringArray(o: JsonObject, k: string): string[] | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (!Array.isArray(v) || !v.every((x) => typeof x === "string")) throw typeErr(k, "[]string");
  return v as string[];
}

export function optStringMap(o: JsonObject, k: string): Record<string, string> | undefined {
  const v = o[k];
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "object" || Array.isArray(v) || !Object.values(v).every((x) => typeof x === "string")) {
    throw typeErr(k, "map[string]string");
  }
  return v as Record<string, string>;
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
    throw typeErr(k, "map[string]*string");
  }
  return v as Record<string, string | null>;
}

export function now(): string {
  return new Date().toISOString();
}

// The byte-capped body readers live in @openfactory/core; they are re-exported
// here for the callers that import them from this module.
export { readBytesCapped, readTextCapped, readTextPrefix } from "@openfactory/core";
