// DO side of live updates (01 §5.4): the Worker opens a hibernatable WebSocket
// to the DO; the DO sends LiveFrame JSON on it. The Worker-side helpers that
// hold long-polls and SSE streams live in src/stream/ (WS5).
import type { LiveFrame } from "../../contracts/do-messages";

export class LiveHub {
  constructor(private readonly ctx: DurableObjectState) {}

  /**
   * Accepts a subscriber socket with tags (for example "team", "run:<id>").
   * The attachment (<= 16 KiB) survives hibernation; use it for cursors and filters.
   * `onOpen` runs in the same synchronous turn, so a backlog sent there loses no wake.
   */
  accept(tags: string[], attachment?: unknown, onOpen?: (ws: WebSocket) => void): Response {
    const pair = new WebSocketPair();
    const client = pair[0];
    const server = pair[1];
    this.ctx.acceptWebSocket(server, tags);
    if (attachment !== undefined) server.serializeAttachment(attachment);
    onOpen?.(server);
    return new Response(null, { status: 101, webSocket: client });
  }

  sockets(tag?: string): WebSocket[] {
    return this.ctx.getWebSockets(tag);
  }

  /** Sends a frame to every socket with the tag that passes the filter. Returns the number sent. */
  broadcast(tag: string, frame: LiveFrame, filter?: (attachment: unknown) => boolean): number {
    const text = JSON.stringify(frame);
    let n = 0;
    for (const ws of this.ctx.getWebSockets(tag)) {
      if (filter && !filter(ws.deserializeAttachment())) continue;
      try {
        ws.send(text);
        n++;
      } catch {
        // Closed socket: the Worker re-subscribes.
      }
    }
    return n;
  }
}

/** True when the request asks for a WebSocket upgrade. */
export function isUpgrade(request: Request): boolean {
  return request.headers.get("upgrade")?.toLowerCase() === "websocket";
}
