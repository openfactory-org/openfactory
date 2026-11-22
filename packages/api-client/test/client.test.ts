import { describe, expect, it } from "vitest";
import {
  ApiError,
  apiBaseUrl,
  backoffMs,
  createOpenFactoryClient,
  errorCode,
  isForbidden,
  isNotFound,
  isUnauthorized,
  paginate,
  parseRetryAfter,
  readSSE,
  runFinished,
  runWaitsForInput,
  shouldRetry,
  unwrap,
  type RunEvent,
} from "../src/index";

type Handler = (req: Request, n: number) => Response | Promise<Response>;

/** A fake fetch that records requests and answers with handler. */
function fakeFetch(handler: Handler): { fetch: typeof fetch; calls: Request[] } {
  const calls: Request[] = [];
  const f = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = new Request(input, init);
    calls.push(req);
    return handler(req, calls.length);
  };
  return { fetch: f as typeof fetch, calls };
}

const json = (v: unknown, status = 200, headers: Record<string, string> = {}): Response =>
  new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json", ...headers } });

const noSleep = async (): Promise<void> => {};

function sseBody(chunks: string[]): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  return new ReadableStream({
    start(c) {
      for (const ch of chunks) c.enqueue(enc.encode(ch));
      c.close();
    },
  });
}

describe("transport", () => {
  it("normalizes the base URL", () => {
    expect(apiBaseUrl("https://x.example.com/")).toBe("https://x.example.com/api/v1");
    expect(apiBaseUrl("https://x.example.com/api/v1/")).toBe("https://x.example.com/api/v1");
    expect(apiBaseUrl("")).toBe("/api/v1");
  });

  it("retries only safe cases", () => {
    expect(shouldRetry("GET", 0)).toBe(true);
    expect(shouldRetry("POST", 0)).toBe(false);
    expect(shouldRetry("POST", 429)).toBe(true);
    expect(shouldRetry("POST", 503)).toBe(true);
    expect(shouldRetry("POST", 500)).toBe(false);
    expect(shouldRetry("PATCH", 502)).toBe(false);
    expect(shouldRetry("DELETE", 504)).toBe(true);
    expect(shouldRetry("GET", 404)).toBe(false);
  });

  it("computes backoff with jitter and Retry-After", () => {
    expect(backoffMs(0, 0, { random: () => 0 })).toBe(250);
    expect(backoffMs(0, 0, { random: () => 1 })).toBe(500);
    expect(backoffMs(10, 0, { random: () => 1 })).toBe(8000);
    expect(backoffMs(0, 5000, {})).toBe(5000);
    expect(backoffMs(0, 999_999, {})).toBe(32_000);
    expect(parseRetryAfter("3")).toBe(3000);
    expect(parseRetryAfter(new Date(Date.now() + 10_000).toUTCString())).toBeGreaterThan(5000);
    expect(parseRetryAfter("nonsense")).toBe(0);
  });

  it("retries 503 and then succeeds, with auth and user agent", async () => {
    const f = fakeFetch((_r, n) =>
      n <= 2
        ? json({ code: "resource_unavailable", title: "busy", status: 503, retryable: true }, 503, { "Retry-After": "0" })
        : json({ uid: "fac_1", name: "ok" }),
    );
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", apiKey: "k1", fetch: f.fetch, sleep: noSleep });
    const got = await c.request<{ name: string }>("GET", "factories/fac_1");
    expect(got.name).toBe("ok");
    expect(f.calls).toHaveLength(3);
    expect(f.calls[0]!.url).toBe("https://of.test/api/v1/factories/fac_1");
    expect(f.calls[0]!.headers.get("Authorization")).toBe("Bearer k1");
    expect(f.calls[0]!.headers.get("User-Agent")).toMatch(/^openfactory-ts\//);
  });

  it("does not retry a POST on 500 and retries a GET up to maxRetries", async () => {
    const f = fakeFetch(() => new Response("", { status: 500 }));
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    await expect(c.request("POST", "runs", { body: { prompt: "x" } })).rejects.toSatisfy((e) => errorCode(e) === "internal_error");
    expect(f.calls).toHaveLength(1);
    const g = fakeFetch(() => new Response("", { status: 500 }));
    const c2 = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: g.fetch, sleep: noSleep, maxRetries: 2 });
    await expect(c2.request("GET", "factories/x")).rejects.toBeInstanceOf(ApiError);
    expect(g.calls).toHaveLength(3);
  });

  it("resends the request body on a retry", async () => {
    const bodies: string[] = [];
    const f = fakeFetch(async (req, n) => {
      bodies.push(await req.text());
      return n === 1 ? new Response("", { status: 429 }) : json({ ok: true });
    });
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    await c.request("POST", "runs", { body: { prompt: "x" } });
    expect(bodies).toEqual(['{"prompt":"x"}', '{"prompt":"x"}']);
  });
});

describe("errors", () => {
  it("parses problem documents and maps statuses", async () => {
    const f = fakeFetch((req) => {
      if (req.url.endsWith("/missing"))
        return json({ type: "about:blank", code: "resource_not_found", title: "nope", detail: "factory fac_x", status: 404, request_id: "req_1" }, 404);
      if (req.url.endsWith("/nested")) return json({ error: { code: "not_authorized", message: "denied" } }, 403);
      return new Response("plain", { status: 401 });
    });
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    const e1 = await c.request("GET", "missing").catch((e: unknown) => e);
    expect(isNotFound(e1)).toBe(true);
    expect((e1 as ApiError).requestId).toBe("req_1");
    expect((e1 as ApiError).message).toBe("openfactory: 404 resource_not_found: nope (factory fac_x)");
    const e2 = await c.request("GET", "nested").catch((e: unknown) => e);
    expect(isForbidden(e2)).toBe(true);
    expect((e2 as ApiError).title).toBe("denied");
    expect(isUnauthorized(await c.request("GET", "me").catch((e: unknown) => e))).toBe(true);
  });

  it("unwraps typed client results", async () => {
    const f = fakeFetch((req) =>
      req.url.endsWith("/me") ? json({ id: "usr_1", role: "user" }) : json({ code: "resource_not_found", title: "x", status: 404 }, 404),
    );
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    const me = await unwrap(c.api.GET("/me"));
    expect(me).toMatchObject({ id: "usr_1" });
    await expect(unwrap(c.api.GET("/factories/{uid}", { params: { path: { uid: "fac_x" } } }))).rejects.toSatisfy(isNotFound);
  });
});

describe("pagination", () => {
  it("walks every page", async () => {
    const pages: Record<string, { items: number[]; page_info: { has_next_page: boolean; next_cursor?: string } }> = {
      "": { items: [1, 2], page_info: { has_next_page: true, next_cursor: "c2" } },
      c2: { items: [3], page_info: { has_next_page: true, next_cursor: "c3" } },
      c3: { items: [], page_info: { has_next_page: false } },
    };
    const seen: string[] = [];
    const p = paginate<number>(async (cursor) => {
      seen.push(cursor);
      return pages[cursor]!;
    });
    expect(await p.collect()).toEqual([1, 2, 3]);
    expect(seen).toEqual(["", "c2", "c3"]);
    const out: number[] = [];
    for await (const n of p) out.push(n);
    expect(out).toEqual([1, 2, 3]);
  });

  it("lists through the client with limit and cursor", async () => {
    const f = fakeFetch((req) => {
      const u = new URL(req.url);
      return u.searchParams.get("cursor") === "n"
        ? json({ items: [{ id: "b" }], page_info: { has_next_page: false } })
        : json({ items: [{ id: "a" }], page_info: { has_next_page: true, next_cursor: "n" } });
    });
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    const items = await c.list<{ id: string }>("runs", { name: "triage" }, 50).collect();
    expect(items.map((i) => i.id)).toEqual(["a", "b"]);
    expect(new URL(f.calls[0]!.url).searchParams.get("limit")).toBe("50");
    expect(new URL(f.calls[0]!.url).searchParams.get("name")).toBe("triage");
  });
});

describe("runs", () => {
  it("knows run states", () => {
    expect(runFinished("SUCCEEDED")).toBe(true);
    expect(runFinished("BLOCKED")).toBe(false);
    expect(runWaitsForInput("BLOCKED")).toBe(true);
  });

  it("pages messages by after", async () => {
    const f = fakeFetch((req) => {
      const after = new URL(req.url).searchParams.get("after");
      return after === "0"
        ? json({ run_id: "r", messages: [{ seq: 1 }, { seq: 2 }], has_more: true, next_after: 2 })
        : json({ run_id: "r", messages: [{ seq: 3 }], has_more: false, next_after: 3 });
    });
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    const msgs = await c.runMessages("r");
    expect(msgs.map((m) => m.seq)).toEqual([1, 2, 3]);
  });

  it("waits for a finished or blocked run", async () => {
    const states = ["QUEUED", "RUNNING", "BLOCKED", "SUCCEEDED"];
    const f = fakeFetch((_r, n) => json({ run_id: "r", state: states[Math.min(n - 1, 3)] }));
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    expect((await c.waitForRun("r", { intervalMs: 1, untilBlocked: true })).state).toBe("BLOCKED");
    const g = fakeFetch((_r, n) => json({ run_id: "r", state: states[Math.min(n - 1, 3)] }));
    const c2 = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: g.fetch, sleep: noSleep });
    expect((await c2.waitForRun("r", { intervalMs: 1 })).state).toBe("SUCCEEDED");
  });

  it("uploads attachments with the content type and name", async () => {
    const f = fakeFetch(() => json({ attachment_id: "att_1", name: "a.txt" }));
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    const a = await c.uploadAttachment("a.txt", "hello", "text/plain");
    expect(a.attachment_id).toBe("att_1");
    expect(f.calls[0]!.headers.get("Content-Type")).toBe("text/plain");
    expect(new URL(f.calls[0]!.url).searchParams.get("name")).toBe("a.txt");
  });
});

describe("SSE", () => {
  it("parses blocks across chunks, comments and CRLF", async () => {
    const events = [];
    for await (const ev of readSSE(sseBody([": hello\n\n", "event: run\nid: 1\ndata: {\"a\"", ":1}\r\n\r\n", "data: x\ndata: y\n\n", "event: done\ndata:"]))) {
      events.push(ev);
    }
    expect(events).toEqual([
      { event: "run", id: "1", data: '{"a":1}' },
      { event: "message", id: "", data: "x\ny" },
    ]);
  });

  it("streams run events and reconnects after the last seq", async () => {
    const f = fakeFetch((req, n) => {
      if (n === 1) {
        return new Response(
          sseBody([
            'event: run\ndata: {"run_id":"r","state":"RUNNING"}\n\n',
            'event: message\nid: 4\ndata: {"seq":4,"role":"assistant","content":"hi"}\n\n',
          ]),
          { headers: { "Content-Type": "text/event-stream" } },
        );
      }
      expect(new URL(req.url).searchParams.get("after")).toBe("4");
      expect(req.headers.get("Last-Event-ID")).toBe("4");
      return new Response(sseBody(['event: done\ndata: {"run_id":"r","state":"SUCCEEDED"}\n\n']), {
        headers: { "Content-Type": "text/event-stream" },
      });
    });
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep });
    const got: RunEvent[] = [];
    for await (const ev of c.streamRunEvents("r", { stopOnIdle: true })) got.push(ev);
    expect(got.map((e) => e.type)).toEqual(["run", "message", "done"]);
    expect(got[1]!.message?.content).toBe("hi");
    expect(got[2]!.run?.state).toBe("SUCCEEDED");
    expect(new URL(f.calls[0]!.url).searchParams.get("stop_on_idle")).toBe("true");
    expect(f.calls[0]!.headers.get("Accept")).toBe("text/event-stream");
  });

  it("gives up after the retries", async () => {
    const f = fakeFetch(() => new Response(sseBody([]), { headers: { "Content-Type": "text/event-stream" } }));
    const c = createOpenFactoryClient({ baseUrl: "https://of.test", fetch: f.fetch, sleep: noSleep, maxRetries: 2 });
    const run = async (): Promise<void> => {
      for await (const _ of c.streamRunEvents("r")) void _;
    };
    await expect(run()).rejects.toThrow(/event stream ended/);
    expect(f.calls).toHaveLength(3);
  });
});
