// A small JSON REST client over fetch for provider APIs (forges, trackers,
// chat services). Port of internal/forge/forgeutil/http.go. It also holds the
// byte-capped body readers that the client uses.
import { OfError, type ErrorCode } from "./errors";

/** Largest response body that is read. A larger 2xx body fails the request. */
export const API_MAX_RESPONSE_BYTES = 32 << 20;

/** Most bytes of a non-2xx response body that are kept for the error message. */
export const API_MAX_ERROR_BODY_BYTES = 64 << 10;

/** Default timeout of one request. */
export const API_TIMEOUT_MS = 30_000;

export type FetchFn = (input: Request | string | URL, init?: RequestInit) => Promise<Response>;

/** A non-2xx response. */
export class ApiError extends Error {
  override readonly name = "ApiError";
  constructor(
    readonly method: string,
    readonly url: string,
    readonly status: number,
    readonly body: string,
  ) {
    super(`${method} ${url}: HTTP ${status}: ${(body.length > 512 ? body.slice(0, 512) + "..." : body).trim()}`);
  }

  /** True for 429 and 5xx: a retry can succeed. */
  get temporary(): boolean {
    return this.status === 429 || this.status >= 500;
  }
}

/** The HTTP status of the first ApiError in the cause chain of err; 0 when there is none. */
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

/** Removes the user info of a URL, for error messages. */
export function redactUrl(u: string): string {
  try {
    const p = new URL(u);
    p.username = "";
    p.password = "";
    return p.toString();
  } catch {
    return u;
  }
}

/** A request body: a string or bytes (sent as JSON), URLSearchParams (a form) or a JSON value. */
export type RequestBody = undefined | null | string | Uint8Array | URLSearchParams | object;

/** What authorize sees of a request. */
export interface AuthorizeRequest {
  method: string;
  url: URL;
  /** The credential passed to the request method, if any. */
  auth?: string;
}

export interface ApiClientOptions {
  /** Root of relative paths. Paths that start with http(s):// are used as given. */
  baseUrl?: string;
  userAgent?: string;
  /** Added to every request. */
  headers?: Record<string, string>;
  /** Sets the credential headers of each request. */
  authorize?: (headers: Headers, req: AuthorizeRequest) => void | Promise<void>;
  fetch?: FetchFn;
  timeoutMs?: number;
}

export type ResponseKind = "json" | "text" | "bytes" | "none";

export interface ApiResponse<T> {
  status: number;
  headers: Headers;
  data: T;
}

export class ApiClient {
  constructor(private readonly opts: ApiClientOptions = {}) {}

  /** Joins the base URL and a path (which may hold a query string). */
  url(path: string): string {
    if (path.startsWith("http://") || path.startsWith("https://")) return path;
    const base = (this.opts.baseUrl ?? "").replace(/\/+$/, "");
    return path === "" ? base : base + "/" + path.replace(/^\/+/, "");
  }

  /** Sends a request and decodes the JSON answer (undefined for an empty body). */
  async json<T = unknown>(method: string, path: string, body?: RequestBody, auth?: string): Promise<T> {
    return (await this.request<T>(method, path, body, "json", auth)).data;
  }

  /** Sends a request and ignores the answer body. */
  async send(method: string, path: string, body?: RequestBody, auth?: string): Promise<void> {
    await this.request(method, path, body, "none", auth);
  }

  /** Sends a request and returns the answer as text ("" for a blank body). */
  async text(method: string, path: string, body?: RequestBody, auth?: string): Promise<string> {
    return (await this.request<string>(method, path, body, "text", auth)).data;
  }

  async bytes(method: string, path: string, body?: RequestBody, auth?: string): Promise<Uint8Array> {
    return (await this.request<Uint8Array>(method, path, body, "bytes", auth)).data;
  }

  /** Sends a request and returns the raw answer text with its status and headers. */
  async raw(
    method: string,
    path: string,
    body?: RequestBody,
    auth?: string,
  ): Promise<{ status: number; headers: Headers; text: string }> {
    const r = await this.send0(method, path, body, auth);
    return { status: r.status, headers: r.headers, text: new TextDecoder().decode(r.data) };
  }

  /**
   * Sends a request and decodes the answer as kind. It throws ApiError for a
   * non-2xx answer and an OfError (resource_unavailable) for a 2xx body over
   * API_MAX_RESPONSE_BYTES.
   */
  async request<T>(
    method: string,
    path: string,
    body: RequestBody,
    kind: ResponseKind,
    auth?: string,
  ): Promise<ApiResponse<T>> {
    const r = await this.send0(method, path, body, auth);
    const buf = r.data;
    const out = (data: unknown): ApiResponse<T> => ({ status: r.status, headers: r.headers, data: data as T });
    if (kind === "none") return out(undefined);
    if (kind === "bytes") return out(buf);
    if (isBlank(buf)) return out(kind === "text" ? "" : undefined);
    const text = new TextDecoder().decode(buf);
    if (kind === "text") return out(text);
    try {
      return out(JSON.parse(text));
    } catch (e) {
      throw new Error(`decode ${method} ${redactUrl(this.url(path))} response: ${(e as Error).message}`, { cause: e });
    }
  }

  private async send0(
    method: string,
    path: string,
    body: RequestBody,
    auth: string | undefined,
  ): Promise<ApiResponse<Uint8Array<ArrayBuffer>>> {
    const target = this.url(path);
    const headers = new Headers();
    for (const [k, v] of Object.entries(this.opts.headers ?? {})) headers.set(k, v);
    let payload: BodyInit | undefined;
    let contentType = "";
    if (body === undefined || body === null) {
      payload = undefined;
    } else if (typeof body === "string") {
      payload = body;
      contentType = "application/json";
    } else if (body instanceof Uint8Array) {
      payload = body as Uint8Array<ArrayBuffer>;
      contentType = "application/json";
    } else if (body instanceof URLSearchParams) {
      payload = body.toString();
      contentType = "application/x-www-form-urlencoded";
    } else {
      payload = JSON.stringify(body);
      contentType = "application/json";
    }
    if (contentType && !headers.has("Content-Type")) headers.set("Content-Type", contentType);
    if (!headers.has("Accept")) headers.set("Accept", "application/json");
    headers.set("User-Agent", this.opts.userAgent || "openfactory");
    if (this.opts.authorize) {
      await this.opts.authorize(headers, { method, url: new URL(target), ...(auth === undefined ? {} : { auth }) });
    }
    const f = this.opts.fetch ?? ((i: Request | string | URL, init?: RequestInit) => fetch(i, init));
    const resp = await f(target, {
      method,
      headers,
      ...(payload === undefined ? {} : { body: payload }),
      signal: AbortSignal.timeout(this.opts.timeoutMs ?? API_TIMEOUT_MS),
    });
    if (resp.status < 200 || resp.status > 299) {
      throw new ApiError(method, redactUrl(target), resp.status, await readTextPrefix(resp, API_MAX_ERROR_BODY_BYTES));
    }
    const data = await readBytesCapped(
      resp,
      API_MAX_RESPONSE_BYTES,
      `${method} ${redactUrl(target)} response body`,
      "resource_unavailable",
    );
    return { status: resp.status, headers: resp.headers, data };
  }
}

function isBlank(buf: Uint8Array): boolean {
  for (const b of buf) if (b !== 0x20 && b !== 0x0a && b !== 0x0d && b !== 0x09) return false;
  return true;
}

/** What readUpTo read: at most max bytes, and whether the body had more. */
interface Capped {
  bytes: Uint8Array<ArrayBuffer>;
  overflow: boolean;
}

/** Reads at most max bytes of a body stream, then cancels the stream if more bytes remain. */
async function readUpTo(r: Request | Response, max: number): Promise<Capped> {
  if (!r.body) return { bytes: new Uint8Array(0), overflow: false };
  const reader = r.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  let overflow = false;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      const room = max - total;
      if (value.length > room) {
        if (room > 0) chunks.push(value.subarray(0, room));
        total += Math.max(room, 0);
        overflow = true;
        break;
      }
      chunks.push(value);
      total += value.length;
    }
  } finally {
    if (overflow) await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
  const bytes = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    bytes.set(c, off);
    off += c.length;
  }
  return { bytes, overflow };
}

/**
 * Reads a request or response body as bytes. It rejects a body longer than
 * max bytes with an OfError (code `code`, default invalid_request) and does
 * not buffer more than max bytes. A Content-Length above max fails before
 * the read.
 */
export async function readBytesCapped(
  r: Request | Response,
  max: number,
  what = "body",
  code: ErrorCode = "invalid_request",
): Promise<Uint8Array<ArrayBuffer>> {
  const fail = (): OfError => new OfError(code, `${what} too large: more than ${max} bytes`);
  const len = Number(r.headers.get("content-length") ?? "");
  if (Number.isFinite(len) && len > max) {
    await r.body?.cancel().catch(() => {});
    throw fail();
  }
  const { bytes, overflow } = await readUpTo(r, max);
  if (overflow) throw fail();
  return bytes;
}

/** readBytesCapped decoded as UTF-8 text. */
export async function readTextCapped(
  r: Request | Response,
  max: number,
  what = "body",
  code: ErrorCode = "invalid_request",
): Promise<string> {
  return new TextDecoder().decode(await readBytesCapped(r, max, what, code));
}

/** The first max bytes of a body as text; the rest is dropped. For error bodies quoted in messages. */
export async function readTextPrefix(r: Request | Response, max: number): Promise<string> {
  return new TextDecoder().decode((await readUpTo(r, max)).bytes);
}
