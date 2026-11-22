// The Open Factory REST client. Port of pkg/sdk on openapi-fetch: a typed
// client of every operation (`client.api`), plus the helpers that pkg/sdk
// adds: retries, pagination iterators, the run event stream (SSE) with
// reconnect, run waiting (polling), messages paging and attachment upload.

import createClient, { type Client } from "openapi-fetch";
import { ApiError, apiErrorFrom, apiErrorFromResponse } from "./errors";
import { paginate, type ListPage, type Paginator } from "./pagination";
import type { components, paths } from "./schema";
import { readSSE, type SSEEvent } from "./sse";
import { apiBaseUrl, backoffMs, createTransport, sleep, type TransportOptions } from "./transport";

export type Run = components["schemas"]["Run"];
export type RunMessage = components["schemas"]["RunMessage"];
export type RunMessages = components["schemas"]["RunMessages"];
export type Attachment = components["schemas"]["Attachment"];
export type RunConversation = components["schemas"]["RunConversation"];

/** API run states that end a run. */
export const FINISHED_RUN_STATES: readonly string[] = ["SUCCEEDED", "FAILED", "ERROR", "CANCELLED"];

/** Reports whether an API run state is finished. */
export function runFinished(state: string): boolean {
  return FINISHED_RUN_STATES.includes(state.toUpperCase());
}

/** Reports whether an API run state waits for a person (BLOCKED). */
export function runWaitsForInput(state: string): boolean {
  return state.toUpperCase() === "BLOCKED";
}

export interface ClientOptions extends TransportOptions {
  /**
   * The server root, for example https://factories.example.com. A trailing
   * /api/v1 is accepted. Empty means the same origin (the dashboard).
   */
  baseUrl?: string;
}

export type QueryValue = string | number | boolean | undefined | null | readonly (string | number)[];

export interface RequestOptions {
  query?: Record<string, QueryValue>;
  /** A JSON value, or raw bytes or a stream (set Content-Type in headers). */
  body?: unknown;
  headers?: Record<string, string>;
  /** The Accept header (default application/json). */
  accept?: string;
  signal?: AbortSignal;
}

function isRawBody(b: unknown): b is BodyInit {
  return (
    typeof b === "string" ||
    b instanceof ArrayBuffer ||
    ArrayBuffer.isView(b) ||
    b instanceof Blob ||
    b instanceof ReadableStream ||
    b instanceof FormData ||
    b instanceof URLSearchParams
  );
}

/** The client of one Open Factory server. */
export class OpenFactoryClient {
  /** The typed openapi-fetch client of /api/v1. Each call returns { data, error, response }. */
  readonly api: Client<paths>;
  readonly baseUrl: string;
  readonly #fetch: typeof fetch;
  readonly #opts: ClientOptions;

  constructor(opts: ClientOptions = {}) {
    this.#opts = opts;
    this.baseUrl = apiBaseUrl(opts.baseUrl ?? "");
    this.#fetch = createTransport(opts);
    this.api = createClient<paths>({ baseUrl: this.baseUrl, fetch: this.#fetch });
  }

  /** The URL of a path relative to /api/v1, with a query. */
  url(path: string, query?: Record<string, QueryValue>): string {
    let u = this.baseUrl + "/" + path.replace(/^\/+/, "");
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(query ?? {})) {
      if (v === undefined || v === null) continue;
      if (Array.isArray(v)) for (const one of v) q.append(k, String(one));
      else q.set(k, String(v));
    }
    const qs = q.toString();
    if (qs) u += "?" + qs;
    return u;
  }

  /** Sends a request with retries and returns the successful Response. Throws ApiError. */
  async raw(method: string, path: string, o: RequestOptions = {}): Promise<Response> {
    const headers = new Headers(o.headers);
    if (!headers.has("Accept")) headers.set("Accept", o.accept ?? "application/json");
    let body: BodyInit | undefined;
    if (o.body !== undefined) {
      if (isRawBody(o.body)) body = o.body;
      else {
        body = JSON.stringify(o.body);
        if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
      }
    }
    const init: RequestInit = { method, headers };
    if (body !== undefined) init.body = body;
    if (o.signal) init.signal = o.signal;
    if (body instanceof ReadableStream) (init as RequestInit & { duplex: string }).duplex = "half";
    const res = await this.#fetch(this.url(path, o.query), init);
    if (res.status >= 300) throw await apiErrorFromResponse(res);
    return res;
  }

  /**
   * Sends a JSON request to a path relative to /api/v1 and returns the decoded
   * answer (undefined for 204). For operations that the typed client does not
   * fit. Throws ApiError.
   */
  async request<T = unknown>(method: string, path: string, o: RequestOptions = {}): Promise<T> {
    const res = await this.raw(method, path, o);
    if (res.status === 204) return undefined as T;
    const text = await res.text();
    if (text === "") return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch (err) {
      throw new Error(`openfactory: decode response: ${String(err)}`, { cause: err });
    }
  }

  /** Iterates every item of a list endpoint ({items, page_info}) page by page. */
  list<T>(path: string, query: Record<string, QueryValue> = {}, pageSize = 0): Paginator<T> {
    return paginate<T>(async (cursor, signal) => {
      const q: Record<string, QueryValue> = { ...query };
      if (pageSize > 0) q["limit"] = pageSize;
      if (cursor) q["cursor"] = cursor;
      const o: RequestOptions = { query: q };
      if (signal) o.signal = signal;
      return this.request<ListPage<T>>("GET", path, o);
    });
  }

  /** Every transcript message of a run with seq > after (pages of 1000). */
  async runMessages(runId: string, after = 0, signal?: AbortSignal): Promise<RunMessage[]> {
    const all: RunMessage[] = [];
    for (;;) {
      const o: RequestOptions = { query: { after, limit: 1000 } };
      if (signal) o.signal = signal;
      const page = await this.request<RunMessages>("GET", `runs/${encodeURIComponent(runId)}/messages`, o);
      all.push(...page.messages);
      const next = page.next_after ?? 0;
      if (!page.has_more || next <= after) return all;
      after = next;
    }
  }

  /**
   * Polls a run until it is finished and returns it. With untilBlocked, it also
   * returns when the run is BLOCKED (the turn is done and the run waits for a person).
   */
  async waitForRun(
    runId: string,
    opts: { intervalMs?: number; untilBlocked?: boolean; signal?: AbortSignal; timeoutMs?: number } = {},
  ): Promise<Run> {
    return pollUntil(
      () => this.request<Run>("GET", `runs/${encodeURIComponent(runId)}`, opts.signal ? { signal: opts.signal } : {}),
      (r) => runFinished(r.state) || (opts.untilBlocked === true && runWaitsForInput(r.state)),
      { ...opts, sleep: this.#opts.sleep ?? sleep },
    );
  }

  /** The normalized conversation of a run. finalMessageOnly returns only the last assistant message. */
  conversation(runId: string, opts: { finalMessageOnly?: boolean } = {}): Promise<RunConversation> {
    return this.request<RunConversation>("GET", `runs/${encodeURIComponent(runId)}/conversation`, {
      query: { scope: opts.finalMessageOnly ? "final_message" : undefined },
    });
  }

  /** Uploads a file for the messages of a run. Use the attachment_id in a message. */
  async uploadAttachment(name: string, data: BodyInit, contentType = "application/octet-stream"): Promise<Attachment> {
    return this.request<Attachment>("POST", "attachments", {
      query: { name },
      body: data,
      headers: { "Content-Type": contentType },
    });
  }

  /**
   * The event stream of a run (GET /runs/{id}/events). It reconnects after a
   * dropped connection and resumes after the last message seq, until the
   * "done" event.
   */
  streamRunEvents(runId: string, opts: RunStreamOptions = {}): AsyncGenerator<RunEvent> {
    return runEventStream(this, runId, opts, this.#opts);
  }

  /** The team event stream (GET /stream): run, message, inbox, workitem and definition events. */
  async *streamTeamEvents(
    opts: { runId?: string; factoryId?: string; topics?: string[]; after?: number; signal?: AbortSignal } = {},
  ): AsyncGenerator<SSEEvent> {
    const ro: RequestOptions = {
      accept: "text/event-stream",
      headers: { "Cache-Control": "no-cache" },
      query: {
        run_id: opts.runId,
        factory_id: opts.factoryId,
        topics: opts.topics?.join(","),
        after: opts.after && opts.after > 0 ? opts.after : undefined,
      },
    };
    if (opts.signal) ro.signal = opts.signal;
    const res = await this.raw("GET", "stream", ro);
    if (!res.body) return;
    yield* readSSE(res.body, opts.signal);
  }
}

/** Creates a client. */
export function createOpenFactoryClient(opts: ClientOptions = {}): OpenFactoryClient {
  return new OpenFactoryClient(opts);
}

/** Throws the error of an openapi-fetch result, or returns its data. */
export async function unwrap<T>(p: Promise<{ data?: T; error?: unknown; response: Response }>): Promise<T> {
  const r = await p;
  if (r.response.status >= 300 || r.error !== undefined) {
    const body = r.error === undefined ? "" : typeof r.error === "string" ? r.error : JSON.stringify(r.error);
    throw apiErrorFrom(r.response.status, r.response.headers, body);
  }
  return r.data as T;
}

/**
 * Calls fn until done(result) is true, waiting intervalMs (default 2000)
 * between calls. Throws on timeout or abort.
 */
export async function pollUntil<T>(
  fn: () => Promise<T>,
  done: (v: T) => boolean,
  opts: {
    intervalMs?: number;
    timeoutMs?: number;
    signal?: AbortSignal;
    sleep?: (ms: number, s?: AbortSignal | null) => Promise<void>;
  } = {},
): Promise<T> {
  const interval = opts.intervalMs && opts.intervalMs > 0 ? opts.intervalMs : 2000;
  const wait = opts.sleep ?? sleep;
  const deadline = opts.timeoutMs ? Date.now() + opts.timeoutMs : Infinity;
  for (;;) {
    const v = await fn();
    if (done(v)) return v;
    if (Date.now() + interval > deadline) throw new Error("openfactory: wait timed out");
    await wait(interval, opts.signal);
  }
}

// ---------- run event stream ----------

/** Event types of a run event stream. */
export const EVENT_RUN = "run";
export const EVENT_MESSAGE = "message";
export const EVENT_DONE = "done";

/** One event of a run stream. */
export interface RunEvent {
  type: string;
  id: string;
  /** Set for "run" and "done". */
  run?: Run;
  /** Set for "message". */
  message?: RunMessage;
  raw: string;
}

export interface RunStreamOptions {
  /** Resume after this message seq. */
  after?: number;
  /** End the stream when the run is BLOCKED and waits for a person. */
  stopOnIdle?: boolean;
  signal?: AbortSignal;
}

function toRunEvent(ev: SSEEvent): RunEvent {
  const out: RunEvent = { type: ev.event, id: ev.id, raw: ev.data };
  let v: unknown;
  try {
    v = JSON.parse(ev.data);
  } catch {
    return out;
  }
  if (v === null || typeof v !== "object") return out;
  if (ev.event === EVENT_RUN || ev.event === EVENT_DONE) out.run = v as Run;
  else if (ev.event === EVENT_MESSAGE) out.message = v as RunMessage;
  return out;
}

async function* runEventStream(
  c: OpenFactoryClient,
  runId: string,
  opts: RunStreamOptions,
  copts: ClientOptions,
): AsyncGenerator<RunEvent> {
  let lastSeq = opts.after ?? 0;
  let retries = 0;
  const maxRetries = Math.max(copts.maxRetries ?? 3, 1);
  const wait = copts.sleep ?? sleep;
  for (;;) {
    const headers: Record<string, string> = { "Cache-Control": "no-cache" };
    if (lastSeq > 0) headers["Last-Event-ID"] = String(lastSeq);
    const ro: RequestOptions = {
      accept: "text/event-stream",
      headers,
      query: { after: lastSeq > 0 ? lastSeq : undefined, stop_on_idle: opts.stopOnIdle ? "true" : undefined },
    };
    if (opts.signal) ro.signal = opts.signal;
    const res = await c.raw("GET", `runs/${encodeURIComponent(runId)}/events`, ro);
    let failure: unknown = new Error("openfactory: event stream ended");
    try {
      if (res.body) {
        for await (const sse of readSSE(res.body, opts.signal)) {
          const ev = toRunEvent(sse);
          if (ev.type === EVENT_MESSAGE && ev.message && ev.message.seq > lastSeq) lastSeq = ev.message.seq;
          retries = 0;
          yield ev;
          if (ev.type === EVENT_DONE) return;
        }
      }
    } catch (err) {
      failure = err;
    }
    if (opts.signal?.aborted) throw opts.signal.reason ?? failure;
    // The connection ended before "done": reconnect from the last seq.
    if (retries >= maxRetries)
      throw failure instanceof ApiError || failure instanceof Error ? failure : new Error(String(failure));
    await wait(backoffMs(retries, 0, copts), opts.signal);
    retries++;
  }
}
