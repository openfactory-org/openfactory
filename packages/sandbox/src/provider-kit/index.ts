// Shared helpers for hosted sandbox providers (daytona, boatdev,
// cloudflare-sandbox): the bootstrap contract of the sandbox image, the
// sandbox name, TaskStatus construction, exit-code parsing, cursor paging and
// a Bearer-token JSON HTTP client.

import { parseRetryAfter, sanitizeTaskId } from "../gate/index";
import { ProviderError, type SandboxReason, type TaskStatus } from "../types";

// ---------- Bootstrap contract (deploy/sandbox/bootstrap.sh) ----------

/** The process the sandbox starts: tini as PID 1, then the bootstrap script. */
export const BOOTSTRAP_ENTRYPOINT: readonly string[] = ["/usr/bin/tini", "-g", "--", "/opt/openfactory/bootstrap.sh"];

/** Directory of the runtime files of the bootstrap. */
export const RUNTIME_DIR = "/run/openfactory";

/** The bootstrap writes the runner PID here. */
export const RUNNER_PID_FILE = `${RUNTIME_DIR}/runner.pid`;

/** Shell command that sends SIGTERM to the runner. */
export const RUNNER_TERM_COMMAND = `kill -TERM $(cat ${RUNNER_PID_FILE})`;

/** Grace added to maxSessionSeconds for the provider TTL (a leak bound). */
export const TTL_GRACE_SECONDS = 900;

/** Name of the sandbox of one task: `of-<sanitized task ID>`, at most 63 characters. */
export function sandboxName(taskId: string): string {
  return `of-${sanitizeTaskId(taskId)}`.slice(0, 63);
}

// ---------- Status ----------

export interface ExitedDetail {
  message?: string | null;
  exitCode?: number;
  endedAt?: string;
}

/** `base` as an exited TaskStatus with reason; empty fields of `detail` are left out. */
export function exitedStatus(
  base: TaskStatus,
  reason: SandboxReason | undefined,
  detail: ExitedDetail = {},
): TaskStatus {
  const out: TaskStatus = { ...base, state: "exited", reason };
  if (detail.message) out.message = detail.message;
  if (detail.exitCode !== undefined) out.exitCode = detail.exitCode;
  if (detail.endedAt) out.endedAt = detail.endedAt;
  return out;
}

/**
 * The exit code named in a process error text, for example "exit status 75",
 * "exited with code 64" or "exit code: 2". Undefined when the text names none.
 */
export function parseExitCode(text: string | null | undefined): number | undefined {
  const m = /exit(?:ed)?(?:\s+with)?(?:\s+(?:status|code))?\s*[:=]?\s*(\d{1,3})/i.exec(text ?? "");
  return m ? Number(m[1]) : undefined;
}

// ---------- Paging ----------

export interface CursorPage<T> {
  items: readonly T[];
  /** Cursor of the next page. Empty or undefined: this was the last page. */
  cursor?: string | null;
}

/** Yields the items of every page. Stops after maxPages pages as a guard against a cursor loop. */
export async function* paginateCursor<T>(
  fetchPage: (cursor: string | undefined) => Promise<CursorPage<T>>,
  maxPages = 1000,
): AsyncGenerator<T> {
  let cursor: string | undefined;
  for (let page = 0; page < maxPages; page++) {
    const p = await fetchPage(cursor);
    yield* p.items;
    cursor = p.cursor || undefined;
    if (!cursor) return;
  }
}

// ---------- HTTP ----------

/** The parts of a failed HTTP response that the provider error mapping reads. */
export interface HttpErrorInfo {
  status: number;
  /** Provider error code from the body, or "". */
  code: string;
  message: string;
  retryAfter?: number;
}

export interface ProviderHttpOptions {
  /** Provider ID; it starts every error message. */
  provider: string;
  /** Base URL of relative paths. A trailing slash is removed. */
  baseUrl: string;
  apiKey: string;
  /** Headers sent with every request. */
  headers?: Record<string, string>;
  fetch?: typeof fetch;
  /** Response headers with the retry delay, first match wins. Default: retry-after. */
  retryAfterHeaders?: readonly string[];
  /** Maps a failed response to a ProviderError. `what` names the operation. */
  toError(e: HttpErrorInfo, what: string): ProviderError;
}

/**
 * A JSON client for a provider REST API with a Bearer token. A network failure
 * becomes ProviderError("unavailable"); a non-2xx response goes through
 * `toError`.
 */
export class ProviderHttpClient {
  readonly baseUrl: string;
  private readonly f: typeof fetch;

  constructor(private readonly o: ProviderHttpOptions) {
    this.baseUrl = o.baseUrl.replace(/\/$/, "");
    this.f = o.fetch ?? ((i, init) => fetch(i, init));
  }

  /** A path that starts with "/" is relative to baseUrl; anything else is an absolute URL. */
  url(pathOrUrl: string): string {
    return pathOrUrl.startsWith("/") ? `${this.baseUrl}${pathOrUrl}` : pathOrUrl;
  }

  async call(
    method: string,
    pathOrUrl: string,
    body?: unknown,
    headers: Record<string, string> = {},
  ): Promise<Response> {
    const url = this.url(pathOrUrl);
    const h: Record<string, string> = {
      authorization: `Bearer ${this.o.apiKey}`,
      accept: "application/json",
      ...this.o.headers,
      ...headers,
    };
    const init: RequestInit = { method, headers: h };
    if (body !== undefined) {
      init.body = JSON.stringify(body);
      h["content-type"] = "application/json";
    }
    try {
      return await this.f(url, init);
    } catch (e) {
      throw new ProviderError("unavailable", `${this.o.provider} ${method} ${pathOf(url)}: ${(e as Error).message}`);
    }
  }

  /** Reads the error body: `{code?, message?: string | string[], error?: string | {code?, message?}}`. */
  async readError(res: Response): Promise<HttpErrorInfo> {
    let code = "";
    let message: string | undefined;
    try {
      const b = (await res.json()) as {
        code?: string;
        message?: string | string[];
        error?: string | { code?: string; message?: string };
      };
      const err = typeof b.error === "object" && b.error !== null ? b.error : undefined;
      code = b.code ?? err?.code ?? "";
      message =
        err?.message ??
        (Array.isArray(b.message) ? b.message.join("; ") : b.message) ??
        (typeof b.error === "string" ? b.error : undefined) ??
        (code || undefined);
    } catch {
      /* not JSON */
    }
    const out: HttpErrorInfo = { status: res.status, code, message: message ?? res.statusText };
    for (const name of this.o.retryAfterHeaders ?? ["retry-after"]) {
      const ra = parseRetryAfter(res.headers.get(name));
      if (ra !== undefined) {
        out.retryAfter = ra;
        break;
      }
    }
    return out;
  }

  async error(res: Response, what: string): Promise<ProviderError> {
    return this.o.toError(await this.readError(res), what);
  }

  /** The JSON body of a 2xx response; else throws the mapped ProviderError. */
  async json<T>(res: Response, what: string): Promise<T> {
    if (!res.ok) throw await this.error(res, what);
    return (await res.json()) as T;
  }
}

function pathOf(url: string): string {
  try {
    return new URL(url).pathname;
  } catch {
    return url;
  }
}
