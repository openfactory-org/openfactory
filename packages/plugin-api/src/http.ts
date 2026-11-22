// Small fetch client for forge REST APIs. Port of internal/forge/forgeutil/http.go.

/** At most this many response bytes are read (the 32 MiB RPC limit). */
export const MAX_RESPONSE_BYTES = 32 << 20;
export const DEFAULT_TIMEOUT_MS = 30_000;

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

export function statusOf(e: unknown): number {
  return e instanceof ApiError ? e.status : 0;
}

export function isNotFound(e: unknown): boolean {
  return statusOf(e) === 404;
}

export type RequestBody = undefined | null | string | Uint8Array | URLSearchParams | object;

export interface ApiClientOptions {
  /** Root, without a trailing slash. Paths that start with http(s):// are used as given. */
  baseUrl: string;
  userAgent?: string;
  /** Sets the credential headers. */
  authorize?: (headers: Headers, method: string, url: URL) => void;
  fetch?: typeof fetch;
  timeoutMs?: number;
}

export interface ApiResponse<T> {
  data: T;
  headers: Headers;
}

export type ResponseKind = "json" | "text" | "bytes" | "none";

export class ApiClient {
  constructor(private readonly opts: ApiClientOptions) {}

  url(path: string): string {
    if (path.startsWith("http://") || path.startsWith("https://")) return path;
    return this.opts.baseUrl.replace(/\/+$/, "") + "/" + path.replace(/^\/+/, "");
  }

  async json<T = unknown>(method: string, path: string, body?: RequestBody): Promise<T> {
    return (await this.request<T>(method, path, body, "json")).data;
  }

  async send(method: string, path: string, body?: RequestBody): Promise<void> {
    await this.request(method, path, body, "none");
  }

  async text(method: string, path: string, body?: RequestBody): Promise<string> {
    return (await this.request<string>(method, path, body, "text")).data;
  }

  async bytes(method: string, path: string, body?: RequestBody): Promise<Uint8Array> {
    return (await this.request<Uint8Array>(method, path, body, "bytes")).data;
  }

  async request<T>(method: string, path: string, body: RequestBody, kind: ResponseKind): Promise<ApiResponse<T>> {
    const headers = new Headers();
    let payload: BodyInit | undefined;
    if (body === undefined || body === null) {
      payload = undefined;
    } else if (typeof body === "string") {
      payload = body;
      headers.set("Content-Type", "application/json");
    } else if (body instanceof Uint8Array) {
      payload = body;
      headers.set("Content-Type", "application/json");
    } else if (body instanceof URLSearchParams) {
      payload = body.toString();
      headers.set("Content-Type", "application/x-www-form-urlencoded");
    } else {
      payload = JSON.stringify(body);
      headers.set("Content-Type", "application/json");
    }
    headers.set("Accept", "application/json");
    headers.set("User-Agent", this.opts.userAgent ?? "openfactory");
    const target = this.url(path);
    this.opts.authorize?.(headers, method, new URL(target));
    const doFetch = this.opts.fetch ?? fetch;
    const res = await doFetch(target, {
      method,
      headers,
      body: payload,
      signal: AbortSignal.timeout(this.opts.timeoutMs ?? DEFAULT_TIMEOUT_MS),
    });
    const buf = new Uint8Array(await res.arrayBuffer());
    if (buf.byteLength > MAX_RESPONSE_BYTES) {
      throw new Error(`${method} ${redactUrl(target)}: response is larger than ${MAX_RESPONSE_BYTES} bytes`);
    }
    if (res.status < 200 || res.status > 299) {
      throw new ApiError(method, redactUrl(target), res.status, new TextDecoder().decode(buf));
    }
    if (kind === "none" || buf.byteLength === 0 || isBlank(buf)) {
      return { data: (kind === "bytes" ? buf : kind === "text" ? "" : undefined) as T, headers: res.headers };
    }
    if (kind === "bytes") return { data: buf as T, headers: res.headers };
    const text = new TextDecoder().decode(buf);
    if (kind === "text") return { data: text as T, headers: res.headers };
    try {
      return { data: JSON.parse(text) as T, headers: res.headers };
    } catch (e) {
      throw new Error(`decode ${method} ${redactUrl(target)} response: ${(e as Error).message}`);
    }
  }
}

function isBlank(buf: Uint8Array): boolean {
  for (const b of buf) if (b !== 0x20 && b !== 0x0a && b !== 0x0d && b !== 0x09) return false;
  return true;
}

/** Removes credentials from a URL for error messages. */
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
