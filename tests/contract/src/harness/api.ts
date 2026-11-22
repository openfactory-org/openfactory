// REST client of the contract suite. Every JSON response is validated against
// the openapi schema of its operation and status; error responses must be
// application/problem+json documents with the Problem schema.
import type { Loose } from "./loose.ts";
import { expect } from "vitest";
import { assertComponent, assertOpenApiResponse, operationFor } from "./schemas.ts";
import { apiUrl, target } from "./target.ts";

export interface ApiResponse<T = Loose> {
  status: number;
  headers: Headers;
  body: T;
  text: string;
}

export interface RequestOptions {
  body?: unknown;
  /** Raw body (sent as is, with content-type application/json unless headers say otherwise). */
  raw?: string;
  query?: Record<string, string | number | boolean | undefined>;
  headers?: Record<string, string>;
  /** Overrides the client key; null sends no Authorization header. */
  key?: string | null;
  /** Skip the schema check (for responses the spec does not describe). */
  noValidate?: boolean;
}

/** An unexpected status. */
export class ApiError extends Error {
  constructor(
    public readonly method: string,
    public readonly path: string,
    public readonly res: ApiResponse,
  ) {
    super(`${method} ${path}: status ${res.status}: ${res.text.slice(0, 800)}`);
    this.name = "ApiError";
  }
}

export class Api {
  constructor(public readonly key?: string) {}

  /** A client with another API key. */
  as(key: string | undefined): Api {
    return new Api(key);
  }

  async request<T = Loose>(method: string, path: string, opts: RequestOptions = {}): Promise<ApiResponse<T>> {
    let url = apiUrl(path);
    if (opts.query) {
      const q = new URLSearchParams();
      for (const [k, v] of Object.entries(opts.query)) if (v !== undefined) q.set(k, String(v));
      const s = q.toString();
      if (s) url += (url.includes("?") ? "&" : "?") + s;
    }
    const headers: Record<string, string> = { accept: "application/json", ...opts.headers };
    const key = opts.key === undefined ? this.key : opts.key;
    if (key) headers.authorization = `Bearer ${key}`;
    let body: string | undefined;
    if (opts.raw !== undefined) body = opts.raw;
    else if (opts.body !== undefined) body = JSON.stringify(opts.body);
    if (body !== undefined && !Object.keys(headers).some((h) => h.toLowerCase() === "content-type"))
      headers["content-type"] = "application/json";
    const res = await fetch(url, { method, headers, body, redirect: "manual" });
    const text = await res.text();
    let parsed: unknown = undefined;
    const ct = res.headers.get("content-type") ?? "";
    if (text && /json/.test(ct)) {
      try {
        parsed = JSON.parse(text);
      } catch {
        throw new Error(
          `${method} ${path}: status ${res.status}: content-type ${ct} but the body is not JSON: ${text.slice(0, 300)}`,
        );
      }
    }
    const out: ApiResponse<T> = { status: res.status, headers: res.headers, body: parsed as T, text };
    if (!opts.noValidate) validateResponse(method, path, out);
    return out;
  }

  /** Sends the request and fails unless the status is `status`. */
  async expect<T = Loose>(status: number, method: string, path: string, opts?: RequestOptions): Promise<T> {
    const res = await this.request<T>(method, path, opts);
    if (res.status !== status) throw new ApiError(method, path, res);
    return res.body;
  }

  get<T = Loose>(path: string, opts?: RequestOptions): Promise<T> {
    return this.expect<T>(200, "GET", path, opts);
  }
  post<T = Loose>(path: string, body?: unknown, status = 200, opts?: RequestOptions): Promise<T> {
    return this.expect<T>(status, "POST", path, { ...opts, body });
  }
  put<T = Loose>(path: string, body?: unknown, status = 200, opts?: RequestOptions): Promise<T> {
    return this.expect<T>(status, "PUT", path, { ...opts, body });
  }
  patch<T = Loose>(path: string, body?: unknown, status = 200, opts?: RequestOptions): Promise<T> {
    return this.expect<T>(status, "PATCH", path, { ...opts, body });
  }
  async del(path: string, status = 204, opts?: RequestOptions): Promise<void> {
    await this.expect(status, "DELETE", path, opts);
  }

  /** Sends the request, expects an error status and returns the checked problem document. */
  async problem(
    status: number,
    method: string,
    path: string,
    opts?: RequestOptions & { code?: string },
  ): Promise<Problem> {
    const res = await this.request(method, path, opts);
    if (res.status !== status) throw new ApiError(method, path, res);
    const p = expectProblem(res);
    if (opts?.code) expect(p.code, `problem code of ${method} ${path}`).toBe(opts.code);
    return p;
  }
}

export interface Problem {
  type: string;
  title: string;
  status: number;
  code: string;
  detail?: string;
  instance?: string;
  request_id?: string;
  retryable: boolean;
  error: { code: string; message: string };
  errors?: { file?: string; field?: string; message: string; line?: number }[];
  [k: string]: unknown;
}

/** Checks the media type and the Problem schema of an error response. */
export function expectProblem(res: ApiResponse): Problem {
  expect(res.headers.get("content-type") ?? "", "error responses use application/problem+json").toMatch(
    /^application\/problem\+json/,
  );
  assertComponent("Problem", res.body);
  const p = res.body as Problem;
  expect(p.status).toBe(res.status);
  expect(p.error.code).toBe(p.code);
  const rid = res.headers.get("x-request-id");
  if (rid && p.request_id) expect(p.request_id).toBe(rid);
  return p;
}

function validateResponse(method: string, path: string, res: ApiResponse): void {
  if (res.status >= 400) {
    // Every documented error is a problem document. 405 (method not allowed)
    // comes from the router before an operation exists.
    if (res.status !== 405 && operationFor(method, path)) expectProblem(res);
    return;
  }
  if (res.body === undefined) return;
  assertOpenApiResponse(method, path, res.status, res.body);
}

/** Polls `fn` until it returns a value that is not undefined/false, or fails after `timeoutMs`. */
export async function waitFor<T>(
  what: string,
  fn: () => Promise<T | undefined | false | null>,
  timeoutMs = 20_000,
  intervalMs = 200,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  let last: unknown;
  for (;;) {
    try {
      const v = await fn();
      if (v !== undefined && v !== false && v !== null) return v as T;
    } catch (e) {
      last = e;
    }
    if (Date.now() > deadline) {
      throw new Error(`timed out after ${timeoutMs} ms waiting for ${what}${last ? `: ${String(last)}` : ""}`);
    }
    await sleep(intervalMs);
  }
}

export function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/** A unique, lower-case name for test resources. */
export function uniq(prefix: string): string {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 7)}`;
}

export { target };
