// A small Server-Sent Events client over fetch.
import type { Loose } from "./loose.ts";
import { apiUrl } from "./target.ts";

export interface SseEvent {
  event: string;
  id?: string;
  data: string;
  json?: Loose;
}

export class SseStream {
  readonly events: SseEvent[] = [];
  status = 0;
  contentType = "";
  private waiters: { pred: (e: SseEvent) => boolean; resolve: (e: SseEvent) => void }[] = [];
  private readonly ac = new AbortController();
  ended = false;
  private endWaiters: (() => void)[] = [];

  static async open(path: string, key: string, headers: Record<string, string> = {}): Promise<SseStream> {
    const s = new SseStream();
    const res = await fetch(apiUrl(path), {
      headers: { accept: "text/event-stream", authorization: `Bearer ${key}`, ...headers },
      signal: s.ac.signal,
    });
    s.status = res.status;
    s.contentType = res.headers.get("content-type") ?? "";
    if (res.status !== 200 || !res.body) {
      const text = await res.text().catch(() => "");
      throw new Error(`SSE ${path}: status ${res.status}: ${text.slice(0, 300)}`);
    }
    void s.pump(res.body);
    return s;
  }

  private async pump(body: ReadableStream<Uint8Array>): Promise<void> {
    const dec = new TextDecoder();
    let buf = "";
    const reader = body.getReader();
    try {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true }).replace(/\r\n/g, "\n");
        let i: number;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const block = buf.slice(0, i);
          buf = buf.slice(i + 2);
          this.parse(block);
        }
      }
    } catch {
      // aborted or reset
    } finally {
      this.ended = true;
      for (const w of this.endWaiters) w();
    }
  }

  private parse(block: string): void {
    let event = "message";
    let id: string | undefined;
    const data: string[] = [];
    let sawField = false;
    for (const line of block.split("\n")) {
      if (line.startsWith(":")) continue;
      sawField = true;
      const c = line.indexOf(":");
      const field = c < 0 ? line : line.slice(0, c);
      const value = c < 0 ? "" : line.slice(c + 1).replace(/^ /, "");
      if (field === "event") event = value;
      else if (field === "id") id = value;
      else if (field === "data") data.push(value);
    }
    if (!sawField) return;
    const ev: SseEvent = { event, data: data.join("\n") };
    if (id !== undefined) ev.id = id;
    try {
      ev.json = JSON.parse(ev.data);
    } catch {
      // not JSON
    }
    this.events.push(ev);
    this.waiters = this.waiters.filter((w) => {
      if (w.pred(ev)) {
        w.resolve(ev);
        return false;
      }
      return true;
    });
  }

  waitEvent(pred: (e: SseEvent) => boolean, timeoutMs = 20_000): Promise<SseEvent> {
    const hit = this.events.find(pred);
    if (hit) return Promise.resolve(hit);
    return new Promise((resolve, reject) => {
      const t = setTimeout(
        () =>
          reject(
            new Error(
              `SSE: no matching event within ${timeoutMs} ms; got ${this.events.map((e) => e.event).join(",")}`,
            ),
          ),
        timeoutMs,
      );
      this.waiters.push({
        pred,
        resolve: (e) => {
          clearTimeout(t);
          resolve(e);
        },
      });
    });
  }

  waitEnd(timeoutMs = 20_000): Promise<void> {
    if (this.ended) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`SSE: stream still open after ${timeoutMs} ms`)), timeoutMs);
      this.endWaiters.push(() => {
        clearTimeout(t);
        resolve();
      });
    });
  }

  close(): void {
    this.ac.abort();
  }
}
