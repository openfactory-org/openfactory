// A stdio bridge to a streamable HTTP MCP endpoint. It relays newline-delimited
// JSON-RPC messages from stdin to the endpoint and writes the answers to
// stdout. Port of mcpBridge in cmd/openfactory/mcp.go.

import type { Writer } from "./framework.ts";

interface RpcEnvelope {
  id?: unknown;
  method?: string;
  result?: { protocolVersion?: string } | undefined;
}

export interface BridgeOptions {
  url: string;
  apiKey: string;
  fetch: typeof fetch;
  out: Writer;
  errLog: Writer;
  signal?: AbortSignal;
}

function envelope(line: string): RpcEnvelope {
  try {
    const v: unknown = JSON.parse(line);
    return v !== null && typeof v === "object" ? (v as RpcEnvelope) : {};
  } catch {
    return {};
  }
}

export class McpBridge {
  readonly #o: BridgeOptions;
  #session = "";
  #protocol = "";

  constructor(options: BridgeOptions) {
    this.#o = options;
  }

  /** Reads lines from input until it ends, forwards them, then closes the session. */
  async run(input: AsyncIterable<string | Uint8Array>): Promise<void> {
    const decoder = new TextDecoder();
    const pending = new Set<Promise<void>>();
    let buf = "";
    const handle = async (raw: string): Promise<void> => {
      const line = raw.trim();
      if (line === "") return;
      const env = envelope(line);
      if (env.method === "initialize") {
        // Initialization establishes the session; finish it before anything else.
        await this.#forward(line, env);
        return;
      }
      const p = this.#forward(line, env).finally(() => pending.delete(p));
      pending.add(p);
    };
    for await (const chunk of input) {
      buf += typeof chunk === "string" ? chunk : decoder.decode(chunk, { stream: true });
      let nl: number;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl);
        buf = buf.slice(nl + 1);
        await handle(line);
      }
    }
    await handle(buf);
    await Promise.all(pending);
    await this.#closeSession();
  }

  async #forward(msg: string, env: RpcEnvelope): Promise<void> {
    try {
      await this.#post(msg, env);
    } catch (err) {
      const text = err instanceof Error ? err.message : String(err);
      this.#o.errLog.write(`openfactory mcp: ${text}\n`);
      if (env.id !== undefined && env.method !== undefined && env.method !== "") {
        this.#write(
          JSON.stringify({ jsonrpc: "2.0", id: env.id, error: { code: -32603, message: "bridge: " + text } }),
        );
      }
    }
  }

  async #post(msg: string, env: RpcEnvelope): Promise<void> {
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      Accept: "application/json, text/event-stream",
      Authorization: `Bearer ${this.#o.apiKey}`,
    };
    if (this.#session !== "") headers["Mcp-Session-Id"] = this.#session;
    if (this.#protocol !== "") headers["Mcp-Protocol-Version"] = this.#protocol;
    const init: RequestInit = { method: "POST", headers, body: msg };
    if (this.#o.signal) init.signal = this.#o.signal;
    const res = await this.#o.fetch(this.#o.url, init);
    const sid = res.headers.get("Mcp-Session-Id");
    if (sid) this.#session = sid;
    if (res.status === 202 || res.status === 204) {
      await res.body?.cancel();
      return;
    }
    if (res.status >= 300) {
      const body = (await res.text()).slice(0, 4096).trim();
      throw new Error(`server returned ${res.status} ${res.statusText}: ${body}`);
    }
    if ((res.headers.get("Content-Type") ?? "").startsWith("text/event-stream")) {
      await this.#readSSE(res, env);
      return;
    }
    const body = await res.text();
    if (body.trim() !== "") this.#deliver(body, env);
  }

  async #readSSE(res: Response, env: RpcEnvelope): Promise<void> {
    if (!res.body) return;
    const decoder = new TextDecoder();
    const reader = res.body.getReader();
    let buf = "";
    let data: string[] = [];
    const flush = (): void => {
      if (data.length > 0) {
        this.#deliver(data.join("\n"), env);
        data = [];
      }
    };
    const line = (l: string): void => {
      if (l === "") flush();
      else if (l.startsWith("data:")) data.push(l.slice(5).replace(/^ /, ""));
    };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let nl: number;
      while ((nl = buf.indexOf("\n")) >= 0) {
        line(buf.slice(0, nl).replace(/\r$/, ""));
        buf = buf.slice(nl + 1);
      }
    }
    if (buf !== "") line(buf.replace(/\r$/, ""));
    flush();
  }

  /** Writes a server message to stdout and records the protocol version of the initialize result. */
  #deliver(msg: string, req: RpcEnvelope): void {
    if (req.method === "initialize") {
      const v = envelope(msg).result?.protocolVersion;
      if (typeof v === "string" && v !== "") this.#protocol = v;
    }
    let out = msg;
    try {
      out = JSON.stringify(JSON.parse(msg));
    } catch {
      // not JSON: write it as it is
    }
    this.#write(out);
  }

  #write(msg: string): void {
    this.#o.out.write(msg + "\n");
  }

  async #closeSession(): Promise<void> {
    if (this.#session === "") return;
    try {
      const res = await this.#o.fetch(this.#o.url, {
        method: "DELETE",
        headers: { "Mcp-Session-Id": this.#session, Authorization: `Bearer ${this.#o.apiKey}` },
      });
      await res.body?.cancel();
    } catch {
      // The session ends on its own.
    }
  }
}
