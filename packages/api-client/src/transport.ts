// The transport of the client: auth and user agent headers, and retries with
// exponential backoff that honor Retry-After. Port of the transport of
// pkg/sdk (client.go).

/** The client version sent in the User-Agent header. */
export const CLIENT_VERSION = "1.0.0";

export interface RetryOptions {
  /** How many times a retryable request is retried (default 3; 0 disables). */
  maxRetries?: number;
  /** Backoff bounds in milliseconds (default 500 .. 8000). */
  minBackoffMs?: number;
  maxBackoffMs?: number;
  /** Replaces the timer, for tests. */
  sleep?: (ms: number, signal?: AbortSignal | null) => Promise<void>;
  /** Replaces Math.random, for tests. */
  random?: () => number;
}

export interface TransportOptions extends RetryOptions {
  /** API key or session token, sent as "Authorization: Bearer <key>". */
  apiKey?: string;
  /** User-Agent prefix. null sends no User-Agent (browsers). */
  userAgent?: string | null;
  /** More headers on every request. */
  headers?: Record<string, string>;
  /** The fetch to use (default globalThis.fetch). */
  fetch?: typeof fetch;
}

/** Waits ms milliseconds, or rejects when signal aborts. */
export function sleep(ms: number, signal?: AbortSignal | null): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason ?? new DOMException("aborted", "AbortError"));
      return;
    }
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = (): void => {
      clearTimeout(t);
      reject(signal?.reason ?? new DOMException("aborted", "AbortError"));
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/**
 * Reports whether a request may be retried. A connection failure (status 0)
 * and 500, 502, 504 are retried only for methods other than POST and PATCH.
 * 429 and 503 mean the request was not processed and are always retried.
 */
export function shouldRetry(method: string, status: number): boolean {
  const m = method.toUpperCase();
  const unsafe = m === "POST" || m === "PATCH";
  if (status === 0) return !unsafe;
  if (status === 429 || status === 503) return true;
  if (status === 500 || status === 502 || status === 504) return !unsafe;
  return false;
}

/** The wait before retry number attempt (0-based): Retry-After (capped), else full jitter in [d/2, d]. */
export function backoffMs(attempt: number, retryAfterMs: number, opts: RetryOptions = {}): number {
  const min = opts.minBackoffMs ?? 500;
  const max = opts.maxBackoffMs ?? 8000;
  if (retryAfterMs > 0) return Math.min(retryAfterMs, max * 4);
  let d = min * 2 ** attempt;
  if (!(d > 0) || d > max) d = max;
  const rnd = (opts.random ?? Math.random)();
  return Math.floor(d / 2 + rnd * (d / 2));
}

function retryAfterOf(res: Response): number {
  const v = res.headers.get("Retry-After");
  if (!v) return 0;
  if (/^\d+$/.test(v.trim())) return Number(v.trim()) * 1000;
  const t = Date.parse(v);
  return Number.isNaN(t) ? 0 : Math.max(0, t - Date.now());
}

/**
 * A fetch that adds the auth and user agent headers and retries. It returns
 * the last answer (also an error answer) and throws only for a connection
 * failure that it cannot retry.
 */
export function createTransport(opts: TransportOptions = {}): typeof fetch {
  const base = opts.fetch ?? ((...a: Parameters<typeof fetch>) => globalThis.fetch(...a));
  const maxRetries = opts.maxRetries ?? 3;
  const wait = opts.sleep ?? sleep;
  const ua = opts.userAgent === null ? null : (opts.userAgent ? opts.userAgent + " " : "") + "openfactory-ts/" + CLIENT_VERSION;
  const transport = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = new Request(input, init);
    if (opts.apiKey && !req.headers.has("Authorization")) req.headers.set("Authorization", "Bearer " + opts.apiKey);
    if (ua !== null && !req.headers.has("User-Agent")) req.headers.set("User-Agent", ua);
    for (const [k, v] of Object.entries(opts.headers ?? {})) if (!req.headers.has(k)) req.headers.set(k, v);
    for (let attempt = 0; ; attempt++) {
      const last = attempt >= maxRetries;
      let res: Response;
      try {
        res = await base(last ? req : req.clone());
      } catch (err) {
        if (req.signal.aborted || last || !shouldRetry(req.method, 0)) throw err;
        await wait(backoffMs(attempt, 0, opts), req.signal);
        continue;
      }
      if (res.status < 300 || last || !shouldRetry(req.method, res.status)) return res;
      const after = retryAfterOf(res);
      await res.body?.cancel().catch(() => undefined);
      await wait(backoffMs(attempt, after, opts), req.signal);
    }
  };
  return transport as typeof fetch;
}

/**
 * The /api/v1 base URL of a server root. A trailing slash and a trailing
 * /api/v1 are accepted. An empty root gives the relative "/api/v1".
 */
export function apiBaseUrl(root: string): string {
  let base = root.replace(/\/+$/, "");
  if (base.endsWith("/api/v1")) base = base.slice(0, -"/api/v1".length);
  return base + "/api/v1";
}
