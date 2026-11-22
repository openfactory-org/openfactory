// A small JSON REST client over fetch (port of forgeutil.Client).
import type { Fetch } from "./types";

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

  /** The request may succeed when retried. */
  get temporary(): boolean {
    return this.status === 429 || this.status >= 500;
  }
}

export function statusCode(e: unknown): number {
  return e instanceof ApiError ? e.status : 0;
}

export type RequestBody = undefined | string | Uint8Array | URLSearchParams | object;

export interface ClientOptions {
  baseUrl?: string;
  fetch?: Fetch;
  userAgent?: string;
  /** Adds credentials to each request. */
  authorize?: (headers: Headers) => void | Promise<void>;
  headers?: Record<string, string>;
  timeoutMs?: number;
}

function redactUrl(u: string): string {
  try {
    const p = new URL(u);
    p.username = "";
    p.password = "";
    return p.toString();
  } catch {
    return u;
  }
}

export class ApiClient {
  constructor(private readonly o: ClientOptions) {}

  url(path: string): string {
    if (path.startsWith("http://") || path.startsWith("https://")) return path;
    const base = (this.o.baseUrl ?? "").replace(/\/+$/, "");
    return path === "" ? base : base + "/" + path.replace(/^\/+/, "");
  }

  /** Sends a request and parses the JSON answer (undefined for an empty body). */
  async json<T = unknown>(method: string, path: string, input?: RequestBody): Promise<T | undefined> {
    const { text } = await this.raw(method, path, input);
    if (text.trim() === "") return undefined;
    try {
      return JSON.parse(text) as T;
    } catch (e) {
      throw new Error(`decode ${method} ${redactUrl(this.url(path))} response: ${(e as Error).message}`);
    }
  }

  /** Sends a request and ignores the answer body. */
  async send(method: string, path: string, input?: RequestBody): Promise<void> {
    await this.raw(method, path, input);
  }

  async raw(method: string, path: string, input?: RequestBody): Promise<{ text: string; headers: Headers }> {
    let body: BodyInit | undefined;
    let contentType = "";
    if (input === undefined || input === null) {
      body = undefined;
    } else if (typeof input === "string") {
      body = input;
      contentType = "application/json";
    } else if (input instanceof Uint8Array) {
      body = input;
      contentType = "application/json";
    } else if (input instanceof URLSearchParams) {
      body = input.toString();
      contentType = "application/x-www-form-urlencoded";
    } else {
      body = JSON.stringify(input);
      contentType = "application/json";
    }
    const u = this.url(path);
    const headers = new Headers();
    for (const [k, v] of Object.entries(this.o.headers ?? {})) headers.set(k, v);
    if (contentType && !headers.has("Content-Type")) headers.set("Content-Type", contentType);
    if (!headers.has("Accept")) headers.set("Accept", "application/json");
    headers.set("User-Agent", this.o.userAgent || "openfactory");
    if (this.o.authorize) await this.o.authorize(headers);
    const f = this.o.fetch ?? fetch;
    const resp = await f(u, {
      method,
      headers,
      ...(body === undefined ? {} : { body }),
      signal: AbortSignal.timeout(this.o.timeoutMs ?? 30_000),
    });
    const text = await resp.text();
    if (resp.status < 200 || resp.status > 299) throw new ApiError(method, redactUrl(u), resp.status, text);
    return { text, headers: resp.headers };
  }
}
