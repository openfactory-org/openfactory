// A Server-Sent Events reader that works in browsers, Node and Workers (fetch
// streams, no EventSource). Port of the stream reader of pkg/sdk (stream.go).

/** One Server-Sent Event. */
export interface SSEEvent {
  /** The event name; "message" when the block has none. */
  event: string;
  /** The id: field of the block, or "". */
  id: string;
  /** The data lines joined by "\n". */
  data: string;
}

/**
 * Reads SSE blocks from a byte stream. Comment-only blocks (keep-alives) are
 * skipped. The generator ends when the stream ends; a partial block at the end
 * is dropped.
 */
export async function* readSSE(body: ReadableStream<Uint8Array>, signal?: AbortSignal): AsyncGenerator<SSEEvent> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  let event = "";
  let id = "";
  let data: string[] = [];
  let hasData = false;
  const onAbort = (): void => {
    reader.cancel(signal?.reason).catch(() => undefined);
  };
  signal?.addEventListener("abort", onAbort, { once: true });
  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      buf += decoder.decode(value, { stream: true });
      let nl: number;
      while ((nl = buf.search(/\r\n|\n|\r/)) >= 0) {
        const sepLen = buf.startsWith("\r\n", nl) ? 2 : 1;
        if (buf[nl] === "\r" && sepLen === 1 && nl === buf.length - 1) break; // wait: maybe \r\n split across chunks
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + sepLen);
        if (line === "") {
          if (event === "" && !hasData) continue;
          yield { event: event || "message", id, data: data.join("\n") };
          event = "";
          id = "";
          data = [];
          hasData = false;
          continue;
        }
        if (line.startsWith(":")) continue;
        const colon = line.indexOf(":");
        const field = colon < 0 ? line : line.slice(0, colon);
        let val = colon < 0 ? "" : line.slice(colon + 1);
        if (val.startsWith(" ")) val = val.slice(1);
        switch (field) {
          case "event":
            event = val.trim();
            break;
          case "id":
            id = val.trim();
            break;
          case "data":
            data.push(val);
            hasData = true;
            break;
        }
      }
    }
  } finally {
    signal?.removeEventListener("abort", onAbort);
    reader.releaseLock();
  }
}

/** Parses the data of an event as JSON, or returns undefined. */
export function sseJSON<T = unknown>(ev: SSEEvent): T | undefined {
  try {
    return JSON.parse(ev.data) as T;
  } catch {
    return undefined;
  }
}
