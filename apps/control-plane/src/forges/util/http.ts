// A small JSON REST client for provider APIs. Port of
// internal/forge/forgeutil/http.go with fetch in place of net/http.

/** Largest provider response that is read. */
export const MAX_RESPONSE_BYTES = 32 << 20;

/** Default timeout of one provider request. */
export const DEFAULT_TIMEOUT_MS = 30_000;

export type FetchFn = (input: Request | string | URL, init?: RequestInit) => Promise<Response>;

/** A non-2xx provider response. */
export class ApiError extends Error {
  override readonly name = "ApiError";
  constructor(
    readonly method: string,
    readonly url: string,
    readonly status: number,
    readonly body: string,
  ) {
    let b = body;
    if (b.length > 512) b = b.slice(0, 512) + "...";
    super(`${method} ${url}: HTTP ${status}: ${b.trim()}`);
  }

  /** Whether the request may succeed when retried. */
  temporary(): boolean {
    return this.status === 429 || this.status >= 500;
  }
}

/** HTTP status of an ApiError (0 when err is not one). Follows `cause` chains. */
export function statusCode(err: unknown): number {
  for (let e: unknown = err, i = 0; e && i < 8; i++) {
    if (e instanceof ApiError) return e.status;
    e = (e as { cause?: unknown }).cause;
  }
  return 0;
}

export function isNotFound(err: unknown): boolean {
  return statusCode(err) === 404;
}

/** A request body: a string (sent verbatim as JSON), URLSearchParams (form) or a JSON value. */
export type Body = string | URLSearchParams | Uint8Array | object | null | undefined;

export interface ClientOptions {
  baseURL: string;
  userAgent?: string;
  /** Added to every request. */
  headers?: Record<string, string>;
  /** Decorates each request with credentials (`auth` is the value passed to do()). */
  authorize?: (headers: Headers, auth: string | undefined) => void | Promise<void>;
  fetch?: FetchFn;
  timeoutMs?: number;
}

export interface RawResult {
  status: number;
  headers: Headers;
  text: string;
}

/** A minimal JSON REST client. */
export class HttpClient {
  readonly baseURL: string;
  private readonly opts: ClientOptions;

  constructor(opts: ClientOptions) {
    this.opts = opts;
    this.baseURL = opts.baseURL;
  }

  /** Joins the base URL and a path (which may hold a query string). */
  url(path: string): string {
    if (path.startsWith("http://") || path.startsWith("https://")) return path;
    return this.baseURL.replace(/\/+$/, "") + "/" + path.replace(/^\/+/, "");
  }

  /** Sends a request and decodes a JSON response (undefined for an empty body). */
  async do<T = unknown>(method: string, path: string, body?: Body, auth?: string): Promise<T> {
    const r = await this.raw(method, path, body, auth);
    if (r.text.trim() === "") return undefined as T;
    try {
      return JSON.parse(r.text) as T;
    } catch (e) {
      throw new Error(`decode ${method} ${redactURL(this.url(path))} response: ${(e as Error).message}`);
    }
  }

  /** Sends a request and returns the raw response (throws ApiError for non-2xx). */
  async raw(method: string, path: string, body?: Body, auth?: string): Promise<RawResult> {
    const u = this.url(path);
    const headers = new Headers();
    for (const [k, v] of Object.entries(this.opts.headers ?? {})) headers.append(k, v);
    let payload: BodyInit | undefined;
    let contentType = "";
    if (body === undefined || body === null) {
      payload = undefined;
    } else if (typeof body === "string") {
      payload = body;
      contentType = "application/json";
    } else if (body instanceof URLSearchParams) {
      payload = body.toString();
      contentType = "application/x-www-form-urlencoded";
    } else if (body instanceof Uint8Array) {
      payload = body as Uint8Array<ArrayBuffer>;
      contentType = "application/json";
    } else {
      payload = JSON.stringify(body);
      contentType = "application/json";
    }
    if (contentType && !headers.has("content-type")) headers.set("content-type", contentType);
    if (!headers.has("accept")) headers.set("accept", "application/json");
    headers.set("user-agent", this.opts.userAgent || "openfactory");
    if (this.opts.authorize) await this.opts.authorize(headers, auth);
    const f = this.opts.fetch ?? ((i: Request | string | URL, init?: RequestInit) => fetch(i, init));
    const signal = AbortSignal.timeout(this.opts.timeoutMs ?? DEFAULT_TIMEOUT_MS);
    const resp = await f(u, { method, headers, ...(payload !== undefined ? { body: payload } : {}), signal });
    let text = await resp.text();
    if (text.length > MAX_RESPONSE_BYTES) text = text.slice(0, MAX_RESPONSE_BYTES);
    if (resp.status < 200 || resp.status > 299) throw new ApiError(method, redactURL(u), resp.status, text);
    return { status: resp.status, headers: resp.headers, text };
  }
}

/** Drops user info from a URL. */
export function redactURL(u: string): string {
  try {
    const p = new URL(u);
    p.username = "";
    p.password = "";
    return p.toString();
  } catch {
    return u;
  }
}
