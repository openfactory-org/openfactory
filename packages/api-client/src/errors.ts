// API errors: the RFC 7807 problem document of /api/v1. Port of the Error
// type of pkg/sdk.

import type { components } from "./schema";

export type Problem = components["schemas"]["Problem"];
export type ValidationError = components["schemas"]["ValidationError"];

/** API error codes. */
export const CODE_INVALID_REQUEST = "invalid_request";
export const CODE_AUTHENTICATION_REQUIRED = "authentication_required";
export const CODE_NOT_AUTHORIZED = "not_authorized";
export const CODE_RESOURCE_NOT_FOUND = "resource_not_found";
export const CODE_CONFLICT = "conflict";
export const CODE_OPERATION_NOT_SUPPORTED = "operation_not_supported";
export const CODE_FEATURE_NOT_AVAILABLE = "feature_not_available";
export const CODE_RESOURCE_UNAVAILABLE = "resource_unavailable";
export const CODE_INTERNAL_ERROR = "internal_error";
export const CODE_EXTERNAL_AUTH_REQUIRED = "external_authentication_required";

/** An error answer of the API. */
export class ApiError extends Error {
  override readonly name = "ApiError";
  readonly status: number;
  readonly code: string;
  readonly type: string;
  readonly title: string;
  readonly detail: string;
  readonly instance: string;
  readonly retryable: boolean;
  /** The ID of the request, for support. */
  readonly requestId: string;
  /** An optional second code for the cause. */
  readonly reason: string;
  /** For external_authentication_required: link an account at provider first, at authUrl. */
  readonly provider: string;
  readonly authUrl: string;
  readonly setupUrl: string;
  readonly errors: ValidationError[];
  /** The Retry-After of the answer in milliseconds, or 0. */
  readonly retryAfterMs: number;
  /** The raw body. */
  readonly body: string;

  constructor(init: {
    status: number;
    code: string;
    type?: string;
    title?: string;
    detail?: string;
    instance?: string;
    retryable?: boolean;
    requestId?: string;
    reason?: string;
    provider?: string;
    authUrl?: string;
    setupUrl?: string;
    errors?: ValidationError[];
    retryAfterMs?: number;
    body?: string;
  }) {
    let msg = init.title ?? "";
    if (init.detail) msg += " (" + init.detail + ")";
    if (msg === "") msg = `HTTP ${init.status}`;
    super(`openfactory: ${init.status} ${init.code}: ${msg}`);
    this.status = init.status;
    this.code = init.code;
    this.type = init.type ?? "";
    this.title = init.title ?? "";
    this.detail = init.detail ?? "";
    this.instance = init.instance ?? "";
    this.retryable = init.retryable ?? false;
    this.requestId = init.requestId ?? "";
    this.reason = init.reason ?? "";
    this.provider = init.provider ?? "";
    this.authUrl = init.authUrl ?? "";
    this.setupUrl = init.setupUrl ?? "";
    this.errors = init.errors ?? [];
    this.retryAfterMs = init.retryAfterMs ?? 0;
    this.body = init.body ?? "";
  }
}

function codeForStatus(status: number): string {
  if (status === 401) return CODE_AUTHENTICATION_REQUIRED;
  if (status === 403) return CODE_NOT_AUTHORIZED;
  if (status === 404) return CODE_RESOURCE_NOT_FOUND;
  if (status === 409) return CODE_CONFLICT;
  if (status >= 500) return CODE_INTERNAL_ERROR;
  return CODE_INVALID_REQUEST;
}

/** The Retry-After header in milliseconds (seconds or an HTTP date), or 0. */
export function parseRetryAfter(value: string | null, now = Date.now()): number {
  if (!value) return 0;
  if (/^\d+$/.test(value.trim())) return Number(value.trim()) * 1000;
  const t = Date.parse(value);
  return Number.isNaN(t) ? 0 : Math.max(0, t - now);
}

const str = (v: unknown): string | undefined => (typeof v === "string" ? v : undefined);

/** Builds an ApiError from an error answer and its body text. */
export function apiErrorFrom(status: number, headers: Headers, body: string): ApiError {
  let doc: Record<string, unknown> = {};
  try {
    const v: unknown = JSON.parse(body);
    if (v !== null && typeof v === "object" && !Array.isArray(v)) doc = v as Record<string, unknown>;
  } catch {
    // not JSON
  }
  let code = str(doc["code"]) ?? "";
  let title = str(doc["title"]);
  if (code === "") {
    // Accept the nested {"error":{"code","message"}} form too.
    const nested = doc["error"];
    if (nested !== null && typeof nested === "object") {
      const n = nested as Record<string, unknown>;
      if (str(n["code"])) {
        code = str(n["code"])!;
        title = str(n["message"]) ?? title;
      }
    }
  }
  if (code === "") code = codeForStatus(status);
  return new ApiError({
    status,
    code,
    type: str(doc["type"]),
    title,
    detail: str(doc["detail"]),
    instance: str(doc["instance"]),
    retryable: doc["retryable"] === true,
    requestId: str(doc["request_id"]) ?? headers.get("X-Request-Id") ?? undefined,
    reason: str(doc["reason"]),
    provider: str(doc["provider"]),
    authUrl: str(doc["auth_url"]),
    setupUrl: str(doc["setup_url"]),
    errors: Array.isArray(doc["errors"]) ? (doc["errors"] as ValidationError[]) : [],
    retryAfterMs: parseRetryAfter(headers.get("Retry-After")),
    body,
  });
}

/** Builds an ApiError from a Response that is not ok. Reads the body. */
export async function apiErrorFromResponse(res: Response): Promise<ApiError> {
  let body = "";
  try {
    body = await res.text();
  } catch {
    // keep the empty body
  }
  return apiErrorFrom(res.status, res.headers, body.slice(0, 1 << 20));
}

/** The API error code of err, or "" when err is not an ApiError. */
export function errorCode(err: unknown): string {
  return err instanceof ApiError ? err.code : "";
}

export const isNotFound = (err: unknown): boolean => errorCode(err) === CODE_RESOURCE_NOT_FOUND;
export const isConflict = (err: unknown): boolean => errorCode(err) === CODE_CONFLICT;
export const isUnauthorized = (err: unknown): boolean => errorCode(err) === CODE_AUTHENTICATION_REQUIRED;
export const isForbidden = (err: unknown): boolean => errorCode(err) === CODE_NOT_AUTHORIZED;
export const isInvalid = (err: unknown): boolean => errorCode(err) === CODE_INVALID_REQUEST;
