// REST handlers: stream (SSE). Owner: WS5. Port of internal/api/stream.go.
// The Worker holds the stream and a 15 s heartbeat. Team events come from
// the IndexDO socket (run, inbox, workitem, definition). With run_id, the
// WorkDO of the run wakes the stream; the Worker then reads the run and its
// new messages.
import { doName, invalid, partitionOf } from "@openfactory/core";
import { callMeta, teamOf, type Handler } from "../http/handler";
import { runView } from "../engine/view";
import type { Run } from "../engine/types";
import { HEARTBEAT_MS } from "../stream/index";
import { readRunMessages } from "./runs";

/** Stream topics accepted by GET /stream?topics= (stream.go). */
export const STREAM_TOPICS = ["run", "message", "inbox", "workitem", "definition"] as const;

/** GET /api/v1/stream */
export const streamEvents: Handler = async (req, env, ctx) => {
  const q = ctx.url.searchParams;
  const topics = q
    .getAll("topics")
    .flatMap((t) => t.split(","))
    .map((t) => t.trim())
    .filter(Boolean);
  const want = topics.length > 0 ? topics : [...STREAM_TOPICS];
  for (const t of want) {
    if (!(STREAM_TOPICS as readonly string[]).includes(t))
      throw invalid(`unknown topic ${JSON.stringify(t)} (use ${STREAM_TOPICS.join(",")})`);
  }
  const runId = q.get("run_id") ?? "";
  const team = teamOf(ctx);
  const factoryId = q.get("factory_id") ?? "";
  let after = Number(q.get("after") ?? "0");
  if (!Number.isInteger(after) || after < 0) throw invalid(`invalid after ${JSON.stringify(q.get("after"))}`);
  const lei = Number(req.headers.get("last-event-id") ?? "");
  if (Number.isInteger(lei) && lei > after) after = lei;

  const work = runId ? env.WORK.getByName(doName.work(team, partitionOf(runId))) : undefined;
  // 403 or 404 before the stream starts; WorkDO.api checks the caller's role for the run's factory.
  let run = work ? ((await work.api(callMeta(ctx), "getRun", { runId }, null)) as Run) : undefined;

  const qs = new URLSearchParams();
  if (runId) qs.set("run", runId);
  else {
    qs.set("topics", want.filter((t) => t !== "message").join(","));
    if (factoryId) qs.set("factory_id", factoryId);
  }
  const stub = work ?? env.INDEX.getByName(doName.index(team));
  const res = await stub.fetch(`https://do${runId ? "/subscribe/run" : "/subscribe/team"}?${qs}`, {
    headers: { Upgrade: "websocket" },
  });
  const ws = res.webSocket;
  if (!ws) return new Response("stream unavailable", { status: 503 });
  ws.accept();

  const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>();
  const w = writable.getWriter();
  const enc = new TextEncoder();
  const pub = (env.OF_PUBLIC_URL ?? "").replace(/\/+$/, "");
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
  const send = (event: string, id: string, data: unknown) =>
    write(`${id ? `id: ${id}\n` : ""}event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
  const hb = setInterval(() => void write(": ping\n\n"), HEARTBEAT_MS);
  void send("ready", "", { topics: want });

  // Run-scoped: replay the messages, then catch up on each wake (one read at a time).
  let lastSeq = after;
  let lastVersion = run?.version ?? 0;
  let busy: Promise<void> = Promise.resolve();
  const catchUp = (withRun: boolean) => {
    if (!work) return;
    busy = busy.then(async () => {
      if (closed) return;
      try {
        if (withRun && want.includes("run")) {
          const r = (await work.getRun(runId)) as Run;
          if (r.version !== lastVersion) {
            lastVersion = r.version;
            run = r;
            await send("run", "", runView(r, pub ? { publicUrl: pub } : {}));
          }
        }
        if (want.includes("message")) {
          for (const m of await readRunMessages(env, work, callMeta(ctx), runId, lastSeq)) {
            if (m.seq <= lastSeq) continue;
            lastSeq = m.seq;
            await send("message", String(m.seq), m);
          }
        }
      } catch (e) {
        await send("error", "", { message: e instanceof Error ? e.message : String(e) });
      }
    });
  };
  if (work) catchUp(false);

  ws.addEventListener("message", (ev) => {
    try {
      const f = JSON.parse(String(ev.data)) as { type: string; event?: string; data?: unknown };
      if (work) {
        if (f.type === "wake") catchUp(true);
        return;
      }
      if (f.type === "event" && f.event && want.includes(f.event)) void send(f.event, "", f.data);
    } catch {
      /* ignore bad frames */
    }
  });
  // A closed DO socket ends the stream; the client reconnects with Last-Event-ID.
  ws.addEventListener("close", stop);
  ws.addEventListener("error", stop);
  req.signal.addEventListener("abort", stop);
  return new Response(readable, {
    headers: { "content-type": "text/event-stream", "cache-control": "no-cache", "x-accel-buffering": "no" },
  });
};
