import createClient, { type Middleware } from "openapi-fetch";
import type { components, paths } from "./schema";

/** Every schema of the embedded OpenAPI document (internal/api/openapi.json). */
export type Schemas = components["schemas"];

export type Factory = Schemas["Factory"];
export type Run = Schemas["Run"];
export type RunMessage = Schemas["RunMessage"];
export type InboxItem = Schemas["InboxItem"];
export type WorkItem = Schemas["WorkItem"];
export type Me = Schemas["Me"];
export type LoginProvider = Schemas["LoginProvider"];
export type ValidationError = Schemas["ValidationError"];

const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

/** Event dispatched on window when any API call answers 401. */
export const UNAUTHORIZED_EVENT = "of:unauthorized";

function origin(): string {
  return typeof window !== "undefined" ? window.location.origin : "http://localhost";
}

/** Problem document returned by the API on errors (RFC 7807 + code). */
export interface Problem {
  type?: string;
  title?: string;
  status?: number;
  detail?: string;
  code?: string;
  errors?: ValidationError[];
  [key: string]: unknown;
}

export class ApiError extends Error {
  readonly status: number;
  readonly problem: Problem;
  constructor(status: number, problem: Problem) {
    super(problem.detail || problem.title || `Request failed (${status})`);
    this.name = "ApiError";
    this.status = status;
    this.problem = problem;
  }
  get validationErrors(): ValidationError[] {
    return Array.isArray(this.problem.errors) ? this.problem.errors : [];
  }
}

/**
 * Session and CSRF handling shared by every request: cookies are always sent,
 * and unsafe methods carry `X-OF-CSRF: 1` (checked by internal/auth for
 * cookie-authenticated requests).
 */
const sessionMiddleware: Middleware = {
  onRequest({ request }) {
    if (!SAFE_METHODS.has(request.method.toUpperCase())) {
      request.headers.set("X-OF-CSRF", "1");
    }
    return request;
  },
  onResponse({ response }) {
    if (response.status === 401 && typeof window !== "undefined") {
      window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    }
    return response;
  },
};

/** Typed REST client for /api/v1, generated from the OpenAPI spec. */
export const api = createClient<paths>({
  baseUrl: `${origin()}/api/v1`,
  credentials: "include",
  // Resolve fetch lazily so tests can stub globalThis.fetch.
  fetch: (req: Request) => globalThis.fetch(req),
});
api.use(sessionMiddleware);

type Result<T> = { data?: T; error?: unknown; response: Response };

/** Unwraps an openapi-fetch result, throwing ApiError on failure. */
export async function unwrap<T>(p: Promise<Result<T>>): Promise<T> {
  const { data, error, response } = await p;
  if (!response.ok) {
    const problem = (error && typeof error === "object" ? error : { detail: String(error ?? "") }) as Problem;
    throw new ApiError(response.status, problem);
  }
  return data as T;
}

async function authFetch(path: string, init: RequestInit): Promise<Response> {
  const headers = new Headers(init.headers);
  if (init.method && !SAFE_METHODS.has(init.method.toUpperCase())) headers.set("X-OF-CSRF", "1");
  return globalThis.fetch(`${origin()}${path}`, { ...init, headers, credentials: "include" });
}

async function problemFrom(res: Response): Promise<Problem> {
  try {
    return (await res.json()) as Problem;
  } catch {
    return { detail: res.statusText };
  }
}

/** Signs in with an API key; internal/auth answers with a session cookie. */
export async function loginWithAPIKey(apiKey: string): Promise<void> {
  const res = await authFetch("/auth/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ api_key: apiKey }),
  });
  if (!res.ok) throw new ApiError(res.status, await problemFrom(res));
}

/** Ends the browser session. */
export async function logout(): Promise<void> {
  const res = await authFetch("/auth/logout", { method: "POST" });
  if (!res.ok && res.status !== 401) throw new ApiError(res.status, await problemFrom(res));
}

/** Fetches the current identity; resolves null when signed out (401). */
export async function fetchMe(): Promise<{ me: Me | null; providers: LoginProvider[] }> {
  const { data, error, response } = await api.GET("/me");
  if (response.status === 401) {
    const providers = ((error as Problem | undefined)?.login_providers as LoginProvider[] | undefined) ?? (await fetchProviders());
    return { me: null, providers };
  }
  if (!response.ok || !data) throw new ApiError(response.status, (error ?? {}) as Problem);
  return { me: data, providers: data.login_providers ?? [] };
}

async function fetchProviders(): Promise<LoginProvider[]> {
  try {
    const { data } = await api.GET("/login-providers");
    return data?.login_providers ?? [];
  } catch {
    return [];
  }
}

/** Absolute URL of an API path, for EventSource. */
export function apiURL(path: string, query?: Record<string, string | number | undefined>): string {
  const u = new URL(`/api/v1${path}`, origin());
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v !== undefined && v !== "") u.searchParams.set(k, String(v));
  }
  return u.toString();
}

/** How many validation errors a one-line error message lists. */
export const MAX_LISTED_ERRORS = 3;

/** One validation error as "file:line field: message". */
export function formatValidationError(e: ValidationError): string {
  const loc = `${e.file ?? ""}${e.line ? `:${e.line}` : ""}`;
  const where = [loc, e.field].filter(Boolean).join(" ");
  return where ? `${where}: ${e.message}` : e.message;
}

/**
 * The text of an error. For an API error with validation errors (a refused
 * definition change), it adds the first few of them, so that the reason is
 * not lost.
 */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const errs = err.validationErrors;
    if (errs.length === 0) return err.message;
    const listed = errs.slice(0, MAX_LISTED_ERRORS).map(formatValidationError).join("; ");
    const more = errs.length > MAX_LISTED_ERRORS ? ` (and ${errs.length - MAX_LISTED_ERRORS} more)` : "";
    return `${err.message}: ${listed}${more}`;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}
