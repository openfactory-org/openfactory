import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { afterEach, describe, expect, it } from "vitest";
import { McpBridge } from "../src/mcp-bridge.ts";

let server: Server | undefined;
afterEach(() => {
  server?.close();
  server = undefined;
});

async function readBody(req: IncomingMessage): Promise<string> {
  let s = "";
  for await (const c of req) s += String(c);
  return s;
}

async function listen(handler: Parameters<typeof createServer>[1]): Promise<string> {
  server = createServer(handler);
  await new Promise<void>((r) => server!.listen(0, "127.0.0.1", r));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

async function* lines(...ls: string[]): AsyncGenerator<string> {
  for (const l of ls) yield l + "\n";
}

describe("McpBridge", () => {
  it("relays JSON and SSE answers, keeps the session and closes it", async () => {
    let sawSession = false;
    let deleted = false;
    const url = await listen(async (req, res) => {
      if (req.headers["authorization"] !== "Bearer k") {
        res.writeHead(401).end("unauthorized");
        return;
      }
      if (req.method === "DELETE") {
        deleted = req.headers["mcp-session-id"] === "s1";
        res.writeHead(204).end();
        return;
      }
      const body = await readBody(req);
      if (body.includes('"initialize"')) {
        res.writeHead(200, { "Mcp-Session-Id": "s1", "Content-Type": "application/json" });
        res.end('{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}');
      } else if (body.includes('"notifications/initialized"')) {
        res.writeHead(202).end();
      } else {
        sawSession = req.headers["mcp-session-id"] === "s1" && req.headers["mcp-protocol-version"] === "2025-06-18";
        res.writeHead(200, { "Content-Type": "text/event-stream" });
        res.end(
          'event: message\ndata: {"jsonrpc":"2.0","id":2,\n\ndata: {"jsonrpc":"2.0","id":2,"result":{"tools":[]}}\n\n',
        );
      }
    });
    let out = "";
    let err = "";
    const bridge = new McpBridge({
      url,
      apiKey: "k",
      fetch,
      out: { write: (c) => void (out += c) },
      errLog: { write: (c) => void (err += c) },
    });
    await bridge.run(
      lines(
        '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}',
        '{"jsonrpc":"2.0","method":"notifications/initialized"}',
        '{"jsonrpc":"2.0","id":2,"method":"tools/list"}',
      ),
    );
    expect(out).toContain('"protocolVersion":"2025-06-18"');
    expect(out).toContain('"tools":[]');
    expect(sawSession).toBe(true);
    expect(deleted).toBe(true);
    expect(err).toBe("");
  });

  it("answers a failed request with a JSON-RPC error, and a failed notification with nothing", async () => {
    const url = await listen((_req, res) => {
      res.writeHead(500).end("boom");
    });
    let out = "";
    let err = "";
    const bridge = new McpBridge({
      url,
      apiKey: "k",
      fetch,
      out: { write: (c) => void (out += c) },
      errLog: { write: (c) => void (err += c) },
    });
    await bridge.run(
      lines('{"jsonrpc":"2.0","id":7,"method":"tools/list"}', '{"jsonrpc":"2.0","method":"notifications/cancelled"}'),
    );
    const msgs = out
      .trimEnd()
      .split("\n")
      .map((l) => JSON.parse(l) as { id: number; error: { code: number; message: string } });
    expect(msgs).toHaveLength(1);
    expect(msgs[0]?.id).toBe(7);
    expect(msgs[0]?.error.code).toBe(-32603);
    expect(msgs[0]?.error.message).toContain("server returned 500");
    expect(err.split("\n").filter(Boolean)).toHaveLength(2);
  });

  it("handles a message that is split across chunks and a last line without newline", async () => {
    const url = await listen(async (req, res) => {
      const b = await readBody(req);
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ jsonrpc: "2.0", id: JSON.parse(b).id, result: {} }));
    });
    let out = "";
    const bridge = new McpBridge({
      url,
      apiKey: "k",
      fetch,
      out: { write: (c) => void (out += c) },
      errLog: { write: () => undefined },
    });
    await bridge.run(
      (async function* () {
        yield '{"jsonrpc":"2.0","id":1,"me';
        yield 'thod":"ping"}\n\n';
        yield '{"jsonrpc":"2.0","id":2,"method":"ping"}';
      })(),
    );
    expect(
      out
        .trimEnd()
        .split("\n")
        .map((l) => (JSON.parse(l) as { id: number }).id)
        .sort(),
    ).toEqual([1, 2]);
  });
});
