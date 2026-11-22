// REST handlers: stream (SSE). Owner: WS5. The Worker holds the stream and a
// 15 s heartbeat; IndexDO (team) or the WorkDO of a run (run_id=) wakes it.
import { doName, partitionOf } from "@openfactory/core";
import type { Handler } from "../http/handler";
import { HEARTBEAT_MS } from "../stream/index";
import { metaOf, teamOf } from "./runs";

/** GET /api/v1/stream */
export const streamEvents: Handler = async (req, env, ctx) => {
  const runId = ctx.url.searchParams.get("run_id");
  const team = teamOf(ctx);
  void metaOf;
  const stub = runId
    ? env.WORK.getByName(doName.work(team, partitionOf(runId)))
    : env.INDEX.getByName(doName.index(team));
  const qs = new URLSearchParams();
  if (runId) qs.set("run", runId);
  const f = ctx.url.searchParams.get("factory_id");
  if (f) qs.set("factory_id", f);
  const res = await stub.fetch(`https://do${runId ? "/subscribe/run" : "/subscribe/team"}?${qs}`, { headers: { Upgrade: "websocket" } });
  const ws = res.webSocket;
  if (!ws) return new Response("stream unavailable", { status: 503 });
  ws.accept();
  const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>();
  const w = writable.getWriter();
  const enc = new TextEncoder();
  let closed = false;
  const stop = () => {
    if (closed) return;
    closed = true;
    clearInterval(hb);
    try {
      ws.close(1000, "done");
    } catch {
      /* closed */
    }
    w.close().catch(() => undefined);
  };
  const write = (s: string) => w.write(enc.encode(s)).catch(stop);
  const hb = setInterval(() => void write(": ping\n\n"), HEARTBEAT_MS);
  void write(": connected\n\n");
  ws.addEventListener("message", (ev) => {
    try {
      const f = JSON.parse(String(ev.data)) as { type: string; id?: string; event?: string; channel?: string; data?: unknown };
      if (f.type === "event") void write(`${f.id ? `id: ${f.id}\n` : ""}event: ${f.event}\ndata: ${JSON.stringify(f.data)}\n\n`);
      else if (f.type === "wake") void write(`event: ${f.channel?.split(":")[0] ?? "wake"}\ndata: ${JSON.stringify({ channel: f.channel })}\n\n`);
    } catch {
      /* ignore bad frames */
    }
  });
  // A closed DO socket ends the stream; the client reconnects with Last-Event-ID.
  ws.addEventListener("close", stop);
  ws.addEventListener("error", stop);
  req.signal.addEventListener("abort", stop);
  return new Response(readable, { headers: { "content-type": "text/event-stream", "cache-control": "no-store" } });
};
