// The result envelope of the DO api() ops (TeamDO.api, IngressDO.deliveries)
// and the one helper that unwraps it. An op returns a plain value (status
// 200), an OpResponse with another status, or an OpError: OfError fields as
// a plain object, so that code, detail, reason, provider, URLs and extra
// survive the RPC boundary.
import { OfError, isErrorCode } from "@openfactory/core";

/** A non-200 result of an op. */
export interface OpResponse {
  $status: number;
  body?: unknown;
  headers?: Record<string, string>;
}

/** The fields of an OfError, as a plain object. */
export interface OpErrorFields {
  code: string;
  detail: string;
  reason?: string;
  provider?: string;
  authUrl?: string;
  setupUrl?: string;
  extra?: Record<string, unknown>;
}

/** An error result of an op. */
export interface OpError {
  $error: OpErrorFields;
}

/** What an op returns: its value, an OpResponse or an OpError. */
export type OpEnvelope<T = unknown> = T | OpResponse | OpError;

export function isOpError(v: unknown): v is OpError {
  return (
    typeof v === "object" && v !== null && typeof (v as OpError).$error === "object" && (v as OpError).$error !== null
  );
}

export function isOpResponse(v: unknown): v is OpResponse {
  return typeof v === "object" && v !== null && typeof (v as OpResponse).$status === "number";
}

/**
 * The OfError of an OpError. An unknown code becomes internal_error. With
 * `op`, the detail starts with "<op>: ".
 */
export function opError(e: OpErrorFields, op?: string): OfError {
  const detail = op ? `${op}: ${e.detail}` : e.detail;
  return new OfError(isErrorCode(e.code) ? e.code : "internal_error", detail, {
    ...(e.reason ? { reason: e.reason } : {}),
    ...(e.provider ? { provider: e.provider } : {}),
    ...(e.authUrl ? { authUrl: e.authUrl } : {}),
    ...(e.setupUrl ? { setupUrl: e.setupUrl } : {}),
    ...(e.extra ? { extra: e.extra } : {}),
  });
}

/**
 * Unwraps the result of a DO api() call: an OpError is thrown as its
 * OfError, an OpResponse gives its body, any other value is returned as is.
 * With `op`, the detail of a thrown error starts with "<op>: ".
 */
export function unwrapOp<T = unknown>(out: unknown, op?: string): T {
  if (isOpError(out)) throw opError(out.$error, op);
  if (isOpResponse(out)) return out.body as T;
  return out as T;
}
