// Long-polls (25 s) and SSE (15 s heartbeat) held in the Worker, woken by a
// DO hibernatable WebSocket (plan rule 8, 01 §5.4). When the socket closes
// (DO reset, deploy), the long-poll returns and the client polls again.

export const MAX_WAIT_S = 25;
export const HEARTBEAT_MS = 15_000;

/** Opens a subscribe socket on a DO. Returns undefined when the DO did not upgrade. */
export async function subscribe(stub: { fetch(input: string, init?: RequestInit): Promise<Response> }, path: string): Promise<WebSocket | undefined> {
  const res = await stub.fetch(`https://do${path}`, { headers: { Upgrade: "websocket" } });
  const ws = res.webSocket ?? undefined;
  if (!ws) return undefined;
  ws.accept();
  return ws;
}

/**
 * Waits for one frame on the socket, the timeout, or the close. Resolves
 * "frame" | "timeout" | "closed". Always closes the socket.
 */
export function waitForWake(ws: WebSocket, waitMs: number, signal?: AbortSignal): Promise<"frame" | "timeout" | "closed"> {
  return new Promise((resolve) => {
    let done = false;
    const finish = (r: "frame" | "timeout" | "closed") => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      try {
        ws.close(1000, "done");
      } catch {
        /* closed */
      }
      resolve(r);
    };
    const timer = setTimeout(() => finish("timeout"), Math.max(0, waitMs));
    ws.addEventListener("message", () => finish("frame"));
    ws.addEventListener("close", () => finish("closed"));
    ws.addEventListener("error", () => finish("closed"));
    signal?.addEventListener("abort", () => finish("closed"));
  });
}

/**
 * A held long-poll: read; if empty, subscribe and wait for a wake, then read
 * again. `read` returns undefined for "nothing yet".
 */
export async function longPoll<T>(opts: {
  read: () => Promise<T | undefined>;
  subscribe: () => Promise<WebSocket | undefined>;
  empty: T;
  waitS: number;
  signal?: AbortSignal;
}): Promise<T> {
  const first = await opts.read();
  if (first !== undefined || opts.waitS <= 0) return first ?? opts.empty;
  const ws = await opts.subscribe();
  if (!ws) return opts.empty;
  const r = await waitForWake(ws, Math.min(opts.waitS, MAX_WAIT_S) * 1000, opts.signal);
  if (r === "frame") return (await opts.read()) ?? opts.empty;
  // Timeout or a closed socket (DO reset): answer empty, the client polls again.
  return opts.empty;
}

export interface SseEvent {
  id?: string;
  event: string;
  data: unknown;
}

/**
 * An SSE response. `backlog(lastId)` returns the events after the cursor;
 * the socket wakes a re-read. A heartbeat comment goes out every 15 s.
 */
export function sseResponse(opts: {
  lastEventId: string;
  backlog: (cursor: string) => Promise<{ events: SseEvent[]; cursor: string }>;
  subscribe: () => Promise<WebSocket | undefined>;
  signal?: AbortSignal;
  heartbeatMs?: number;
}): Response {
  const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>();
  const w = writable.getWriter();
  const enc = new TextEncoder();
  let closed = false;
  let ws: WebSocket | undefined;
  const stop = () => {
    if (closed) return;
    closed = true;
    clearInterval(hb);
    try {
      ws?.close(1000, "done");
    } catch {
      /* closed */
    }
    w.close().catch(() => undefined);
  };
  const write = (s: string) => w.write(enc.encode(s)).catch(() => stop());
  const hb = setInterval(() => void write(": ping\n\n"), opts.heartbeatMs ?? HEARTBEAT_MS);
  let cursor = opts.lastEventId;
  let reading = Promise.resolve();
  const flush = () => {
    reading = reading.then(async () => {
      if (closed) return;
      const b = await opts.backlog(cursor);
      cursor = b.cursor;
      for (const e of b.events) {
        await write(`${e.id ? `id: ${e.id}\n` : ""}event: ${e.event}\ndata: ${JSON.stringify(e.data)}\n\n`);
      }
    });
    return reading;
  };
  void (async () => {
    try {
      ws = await opts.subscribe();
      await flush();
      if (!ws) return stop();
      ws.addEventListener("message", () => void flush());
      ws.addEventListener("close", stop);
      ws.addEventListener("error", stop);
    } catch {
      stop();
    }
  })();
  opts.signal?.addEventListener("abort", stop);
  return new Response(readable, {
    headers: { "content-type": "text/event-stream", "cache-control": "no-store", connection: "keep-alive" },
  });
}
