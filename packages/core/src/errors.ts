// Frozen contract (C0). API error codes and the error type every package throws.
//
// Port of internal/api/errors.go (codes, HTTP status, title, retryable, RFC 7807
// problem body) and of the internal/core sentinels (ErrNotFound, ErrConflict,
// ErrInvalid, ErrUnauthorized, ErrForbidden, ErrUnavailable, ...), which map to
// these codes.

export const ERROR_CODES = [
  "invalid_request",
  "authentication_required",
  "not_authorized",
  "resource_not_found",
  "conflict",
  "operation_not_supported",
  "feature_not_available",
  "integration_not_configured",
  "integration_disabled",
  "resource_unavailable",
  "internal_error",
  "external_authentication_required",
] as const;

export type ErrorCode = (typeof ERROR_CODES)[number];

export interface ErrorSpec {
  readonly status: number;
  readonly title: string;
  readonly retryable: boolean;
}

/** Same values as errorSpecs in internal/api/errors.go. */
export const ERROR_SPECS: Readonly<Record<ErrorCode, ErrorSpec>> = {
  invalid_request: { status: 400, title: "The request contains invalid or missing parameters.", retryable: false },
  authentication_required: { status: 401, title: "A valid API key is required.", retryable: false },
  not_authorized: { status: 403, title: "You do not have permission to perform this operation.", retryable: false },
  resource_not_found: { status: 404, title: "The referenced resource does not exist.", retryable: false },
  conflict: { status: 409, title: "The request conflicts with the current state of the resource.", retryable: false },
  operation_not_supported: {
    status: 422,
    title: "The operation is not supported for this resource or state.",
    retryable: false,
  },
  feature_not_available: { status: 501, title: "This feature is not available on this server.", retryable: false },
  integration_not_configured: { status: 424, title: "The integration setup is incomplete.", retryable: false },
  integration_disabled: { status: 424, title: "The integration is disabled.", retryable: false },
  resource_unavailable: { status: 503, title: "A required resource is temporarily unavailable.", retryable: true },
  internal_error: { status: 500, title: "An unexpected server error occurred.", retryable: true },
  external_authentication_required: {
    status: 401,
    title: "A linked account at an outside provider is required.",
    retryable: false,
  },
};

/**
 * The Go sentinel errors (internal/core) and the code each maps to.
 * Code that ports a Go `errors.Is(err, core.ErrX)` check compares `err.code`.
 */
export const SENTINEL_CODES = {
  ErrNotFound: "resource_not_found",
  ErrConflict: "conflict",
  ErrStale: "conflict",
  ErrInvalid: "invalid_request",
  ErrUnauthorized: "authentication_required",
  ErrForbidden: "not_authorized",
  ErrUnavailable: "resource_unavailable",
  ErrAtCapacity: "resource_unavailable",
  ErrIntegrationDisabled: "integration_disabled",
  ErrOperationNotSupported: "operation_not_supported",
  ErrExternalAuthRequired: "external_authentication_required",
} as const satisfies Record<string, ErrorCode>;

export function isErrorCode(s: string): s is ErrorCode {
  return (ERROR_CODES as readonly string[]).includes(s);
}

export interface OfErrorOptions {
  /** An optional second code for the cause (for example "no_linked_account", "stale"). */
  reason?: string;
  /** The outside provider the error is about. */
  provider?: string;
  /** Where the user authorizes the provider. */
  authUrl?: string;
  /** Where an admin completes the setup. */
  setupUrl?: string;
  /** More top-level members of the problem body (for example "errors"). */
  extra?: Record<string, unknown>;
  cause?: unknown;
}

const MESSAGE_SEP = ": ";

/**
 * The error type of the control plane. `message` is `<code>` or
 * `<code>: <detail>` (the Go Error() text), so that an error that crossed a
 * Workers RPC boundary as a plain Error can be rebuilt with `toOfError`.
 */
export class OfError extends Error {
  override readonly name = "OfError";
  readonly code: ErrorCode;
  readonly detail: string;
  readonly reason: string | undefined;
  readonly provider: string | undefined;
  readonly authUrl: string | undefined;
  readonly setupUrl: string | undefined;
  readonly extra: Record<string, unknown> | undefined;

  constructor(code: ErrorCode, detail = "", opts: OfErrorOptions = {}) {
    super(
      detail === "" ? code : code + MESSAGE_SEP + detail,
      opts.cause === undefined ? undefined : { cause: opts.cause },
    );
    this.code = code;
    this.detail = detail;
    this.reason = opts.reason;
    this.provider = opts.provider;
    this.authUrl = opts.authUrl;
    this.setupUrl = opts.setupUrl;
    this.extra = opts.extra;
  }

  get status(): number {
    return ERROR_SPECS[this.code].status;
  }

  get retryable(): boolean {
    return ERROR_SPECS[this.code].retryable;
  }
}

export const notFound = (what: string): OfError => new OfError("resource_not_found", `${what} not found`);
export const invalid = (detail: string, extra?: Record<string, unknown>): OfError =>
  new OfError("invalid_request", detail, extra ? { extra } : {});
export const conflict = (detail: string): OfError => new OfError("conflict", detail);
export const forbidden = (detail: string): OfError => new OfError("not_authorized", detail);
export const unauthorized = (detail = ""): OfError => new OfError("authentication_required", detail);
export const unavailable = (detail: string): OfError => new OfError("resource_unavailable", detail);
export const notImplemented = (detail = "not implemented yet"): OfError => new OfError("feature_not_available", detail);

export function isOfError(e: unknown, code?: ErrorCode): e is OfError {
  return e instanceof OfError && (code === undefined || e.code === code);
}

/**
 * Maps any thrown value to an OfError. A plain Error whose message starts
 * with a known code (an OfError that crossed RPC) keeps its code. Anything
 * else is internal_error; `known` tells the caller to log it.
 */
export function toOfError(e: unknown): { error: OfError; known: boolean } {
  if (e instanceof OfError) return { error: e, known: true };
  if (e instanceof Error) {
    const msg = e.message;
    const i = msg.indexOf(MESSAGE_SEP);
    const head = i < 0 ? msg : msg.slice(0, i);
    if (isErrorCode(head)) {
      return { error: new OfError(head, i < 0 ? "" : msg.slice(i + MESSAGE_SEP.length), { cause: e }), known: true };
    }
  }
  return { error: new OfError("internal_error", "", { cause: e }), known: false };
}

/** The RFC 7807 problem body (internal/api/errors.go Problem). */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
  code: ErrorCode;
  error: { code: ErrorCode; message: string };
  retryable: boolean;
  request_id?: string;
  reason?: string;
  provider?: string;
  auth_url?: string;
  setup_url?: string;
  [extra: string]: unknown;
}

export function problemBody(err: OfError, ctx: { instance?: string; requestId?: string } = {}): Problem {
  const spec = ERROR_SPECS[err.code];
  const p: Problem = {
    type: "/errors/" + err.code.replaceAll("_", "-"),
    title: spec.title,
    status: spec.status,
    code: err.code,
    error: { code: err.code, message: err.detail === "" ? spec.title : `${spec.title} (${err.detail})` },
    retryable: spec.retryable,
  };
  if (err.detail !== "") p.detail = err.detail;
  if (ctx.instance) p.instance = ctx.instance;
  if (ctx.requestId) p.request_id = ctx.requestId;
  if (err.reason) p.reason = err.reason;
  if (err.provider) p.provider = err.provider;
  if (err.authUrl) p.auth_url = err.authUrl;
  if (err.setupUrl) p.setup_url = err.setupUrl;
  for (const [k, v] of Object.entries(err.extra ?? {})) {
    if (!(k in p)) p[k] = v;
  }
  return p;
}

/** A problem+json Response, with WWW-Authenticate on 401 (Go writeProblem). */
export function problemResponse(err: OfError, ctx: { instance?: string; requestId?: string } = {}): Response {
  const body = problemBody(err, ctx);
  const headers = new Headers({ "content-type": "application/problem+json" });
  if (body.status === 401) headers.set("www-authenticate", 'Bearer realm="openfactory"');
  if (ctx.requestId) headers.set("x-request-id", ctx.requestId);
  return new Response(JSON.stringify(body) + "\n", { status: body.status, headers });
}
