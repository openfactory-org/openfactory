// A fake runner that speaks /runner/v1. Every request and response body is
// validated against schemas/runner/v1.
import type { Loose } from "./loose.ts";
import { assertRunner } from "./schemas.ts";
import { target } from "./target.ts";

/** The task variables that a sandbox gives the runner. */
export interface TaskEnv {
  OF_CONTROL_URL: string;
  OF_RUN_ID: string;
  OF_SESSION: string;
  OF_BOOTSTRAP_TOKEN: string;
  [k: string]: string;
}

export interface RunnerResponse<T = Loose> {
  status: number;
  body: T;
  text: string;
}

export class RunnerHttpError extends Error {
  constructor(
    public readonly path: string,
    public readonly res: RunnerResponse,
  ) {
    super(`runner ${path}: status ${res.status}: ${res.text.slice(0, 500)}`);
    this.name = "RunnerHttpError";
  }
}

export interface Command {
  seq: number;
  type: "turn.start" | "message.deliver" | "turn.interrupt" | "session.stop" | "session.cancel";
  data?: Loose;
}

export interface StartResponse {
  session_token: string;
  run_id: string;
  session: number;
  harness: {
    type: string;
    mcp_servers?: Record<string, { url: string; auth?: string; headers?: Record<string, string> }>;
  };
  env: Record<string, string> | null;
  [k: string]: unknown;
}

let instanceCounter = 0;

export class FakeRunner {
  token = "";
  start_: StartResponse | undefined;
  private seq = 0;
  private lastCommand = 0;
  readonly version: string;

  constructor(
    public readonly env: TaskEnv,
    opts: { version?: string } = {},
  ) {
    this.version = opts.version ?? target.runnerVersion;
  }

  get controlUrl(): string {
    return this.env.OF_CONTROL_URL.replace(/\/+$/, "");
  }
  get runId(): string {
    return this.env.OF_RUN_ID;
  }

  /** A raw call. Error bodies are validated as ErrorBody. */
  async call<T = Loose>(
    method: string,
    path: string,
    opts: { body?: unknown; token?: string; version?: string; query?: Record<string, string> } = {},
  ): Promise<RunnerResponse<T>> {
    let url = this.controlUrl + path;
    if (opts.query) url += "?" + new URLSearchParams(opts.query).toString();
    const headers: Record<string, string> = {
      accept: "application/json",
      authorization: `Bearer ${opts.token ?? this.token}`,
      "OF-Runner-Version": opts.version ?? this.version,
    };
    if (opts.body !== undefined) headers["content-type"] = "application/json";
    const res = await fetch(url, {
      method,
      headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
    });
    const text = await res.text();
    let body: unknown = undefined;
    if (text) {
      try {
        body = JSON.parse(text);
      } catch {
        body = undefined;
      }
    }
    if (res.status >= 400 && body !== undefined) assertRunner("session", "ErrorBody", body);
    return { status: res.status, body: body as T, text };
  }

  private async ok<T>(method: string, path: string, opts: Parameters<FakeRunner["call"]>[2] = {}): Promise<T> {
    const r = await this.call<T>(method, path, opts);
    if (r.status < 200 || r.status >= 300) throw new RunnerHttpError(path, r);
    return r.body;
  }

  startRequest(overrides: Record<string, unknown> = {}): Record<string, unknown> {
    instanceCounter += 1;
    return {
      instance_id: `contract-${process.pid}-${instanceCounter}`,
      runner_version: this.version,
      sandbox_id: `sb-${this.runId}-${this.env.OF_SESSION}`,
      platform: { os: "linux", arch: "amd64" },
      ...overrides,
    };
  }

  /** POST /runner/v1/session/start with the bootstrap token. */
  async start(overrides: Record<string, unknown> = {}): Promise<StartResponse> {
    const req = this.startRequest(overrides);
    assertRunner("session", "StartRequest", req);
    const res = await this.call<StartResponse>("POST", "/runner/v1/session/start", {
      body: req,
      token: this.env.OF_BOOTSTRAP_TOKEN,
      version: String(req.runner_version),
    });
    if (res.status !== 200) throw new RunnerHttpError("/runner/v1/session/start", res);
    assertRunner("session", "StartResponse", res.body);
    this.token = res.body.session_token;
    this.start_ = res.body;
    return res.body;
  }

  /** Sends events with the next sequence numbers. Returns the ack. */
  async emit(events: { type: string; data?: unknown; turn_id?: string }[]): Promise<number> {
    const batch = {
      events: events.map((e) => {
        this.seq += 1;
        const ev: Record<string, unknown> = { seq: this.seq, type: e.type, at: new Date().toISOString() };
        if (e.data !== undefined) ev.data = e.data;
        if (e.turn_id) ev.turn_id = e.turn_id;
        return ev;
      }),
    };
    return this.sendBatch(batch);
  }

  /** Sends a raw batch (for duplicate and gap tests). */
  async sendBatch(batch: { events: Record<string, unknown>[] }): Promise<number> {
    assertRunner("events", "EventBatch", batch);
    const body = await this.ok<{ ack: number }>("POST", "/runner/v1/session/events", { body: batch });
    assertRunner("events", "EventAck", body);
    return body.ack;
  }

  get lastSeq(): number {
    return this.seq;
  }

  async ready(capabilities: string[] = ["interrupt"]): Promise<number> {
    return this.emit([{ type: "session.ready", data: { capabilities } }]);
  }

  /** One commands poll after the last seen command. */
  async commands(waitSeconds = 5): Promise<Command[]> {
    const body = await this.ok<{ commands: Command[] | null }>("GET", "/runner/v1/session/commands", {
      query: { after: String(this.lastCommand), wait: String(waitSeconds) },
    });
    assertRunner("commands", "CommandsResponse", body);
    const cmds = body.commands ?? [];
    for (const c of cmds) {
      if (c.type === "turn.start") assertRunner("commands", "TurnStartData", c.data);
      if (c.type === "message.deliver") assertRunner("commands", "MessageDeliverData", c.data);
      if (c.seq > this.lastCommand) this.lastCommand = c.seq;
    }
    return cmds;
  }

  /** Polls until a command of one of `types` arrives. Other commands are returned in `skipped`. */
  async nextCommand(types: Command["type"][], timeoutMs = 30_000): Promise<Command> {
    const deadline = Date.now() + timeoutMs;
    for (;;) {
      const left = Math.max(1, Math.min(5, Math.floor((deadline - Date.now()) / 1000)));
      const cmds = await this.commands(left);
      const hit = cmds.find((c) => types.includes(c.type));
      if (hit) return hit;
      if (Date.now() > deadline)
        throw new Error(`run ${this.runId}: no ${types.join("/")} command within ${timeoutMs} ms`);
    }
  }

  /** Emits a complete turn: turn.started, an assistant message, turn.ended. */
  async completeTurn(
    turnId: string,
    reply: string,
    outcome: "done" | "error" | "needs_input" = "done",
  ): Promise<number> {
    const ended: Record<string, unknown> = { outcome };
    if (outcome === "done") ended.result = reply;
    if (outcome === "error") {
      ended.failure_code = "agent_failed";
      ended.message = reply;
    }
    assertRunner("events", "MessageData", { role: "assistant", content: reply });
    assertRunner("events", "TurnEndedData", ended);
    return this.emit([
      { type: "turn.started", turn_id: turnId },
      { type: "message", turn_id: turnId, data: { role: "assistant", content: reply } },
      { type: "turn.ended", turn_id: turnId, data: ended },
    ]);
  }

  /** Emits idle with a deadline. */
  async idle(seconds = 600): Promise<number> {
    const data = { deadline: new Date(Date.now() + seconds * 1000).toISOString() };
    assertRunner("events", "IdleData", data);
    return this.emit([{ type: "idle", data }]);
  }

  async secrets(): Promise<Loose> {
    const body = await this.ok("GET", "/runner/v1/session/secrets");
    assertRunner("session", "SecretsResponse", body);
    return body;
  }

  async refreshToken(): Promise<string> {
    const body = await this.ok<{ session_token: string }>("POST", "/runner/v1/session/token", { body: {} });
    assertRunner("session", "TokenResponse", body);
    this.token = body.session_token;
    return body.session_token;
  }

  async conversation(): Promise<Loose> {
    const body = await this.ok("GET", "/runner/v1/session/conversation");
    assertRunner("conversation", "Conversation", body);
    return body;
  }

  async end(
    reason: "idle" | "stopped" | "cancelled" | "setup_failed" | "deadline" | "fatal",
    extra: Record<string, unknown> = {},
  ): Promise<RunnerResponse> {
    const req = { reason, last_event_seq: this.seq, ...extra };
    assertRunner("session", "EndRequest", req);
    return this.call("POST", "/runner/v1/session/end", { body: req });
  }

  /** Calls a tool of the factory MCP server of the run (stateless JSON-RPC over HTTP). */
  async mcp<T = Loose>(tool: string, args: Record<string, unknown>): Promise<T> {
    const server = this.start_?.harness.mcp_servers?.factory;
    if (!server) throw new Error(`run ${this.runId}: the start response has no factory MCP server`);
    const headers: Record<string, string> = {
      "content-type": "application/json",
      accept: "application/json, text/event-stream",
      ...(server.headers ?? {}),
    };
    if (!server.auth || server.auth === "session_token") headers.authorization = `Bearer ${this.token}`;
    const res = await fetch(server.url, {
      method: "POST",
      headers,
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: Date.now(),
        method: "tools/call",
        params: { name: tool, arguments: args },
      }),
    });
    const text = await res.text();
    if (res.status !== 200) throw new Error(`mcp ${tool}: status ${res.status}: ${text.slice(0, 400)}`);
    const msg = parseRpc(text);
    if (msg.error) throw new Error(`mcp ${tool}: ${JSON.stringify(msg.error)}`);
    const result = msg.result as {
      isError?: boolean;
      content?: { type: string; text?: string }[];
      structuredContent?: unknown;
    };
    const txt = result.content?.find((c) => c.type === "text")?.text ?? "";
    if (result.isError) throw new Error(`mcp ${tool}: tool error: ${txt}`);
    if (result.structuredContent !== undefined) return result.structuredContent as T;
    try {
      return JSON.parse(txt) as T;
    } catch {
      return txt as T;
    }
  }
}

function parseRpc(text: string): { result?: unknown; error?: unknown } {
  const t = text.trim();
  if (t.startsWith("{")) return JSON.parse(t) as { result?: unknown; error?: unknown };
  // text/event-stream: take the last data line.
  const data = t
    .split("\n")
    .filter((l) => l.startsWith("data:"))
    .map((l) => l.slice(5).trim());
  const last = data[data.length - 1];
  if (!last) throw new Error(`mcp: unreadable response: ${t.slice(0, 200)}`);
  return JSON.parse(last) as { result?: unknown; error?: unknown };
}
