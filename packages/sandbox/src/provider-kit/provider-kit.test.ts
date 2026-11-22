import { describe, expect, it } from "vitest";
import { ProviderError, parseProviderError, type TaskStatus } from "../types";
import {
  ProviderHttpClient,
  exitedStatus,
  paginateCursor,
  parseExitCode,
  sandboxName,
  type HttpErrorInfo,
} from "./index";

describe("sandboxName", () => {
  it.each([
    ["run_abc.1", "of-run-abc-1"],
    ["RUN_X.2", "of-run-x-2"],
    ["r_" + "a".repeat(80) + ".1", ("of-r-" + "a".repeat(80)).slice(0, 63)],
  ])("%s → %s", (id, want) => {
    expect(sandboxName(id)).toBe(want);
    expect(sandboxName(id).length).toBeLessThanOrEqual(63);
  });
});

describe("exitedStatus", () => {
  const base: TaskStatus = { taskId: "t", state: "running", sandboxId: "s" };
  it("sets the reason and the given fields only", () => {
    expect(
      exitedStatus(base, "process_failed", { message: "boom", exitCode: 1, endedAt: "2026-01-01T00:00:00Z" }),
    ).toEqual({
      ...base,
      state: "exited",
      reason: "process_failed",
      message: "boom",
      exitCode: 1,
      endedAt: "2026-01-01T00:00:00Z",
    });
    expect(exitedStatus(base, "evicted", { message: null, exitCode: 0 })).toEqual({
      ...base,
      state: "exited",
      reason: "evicted",
      exitCode: 0,
    });
    expect(exitedStatus(base, "cancelled")).not.toHaveProperty("message");
  });
});

describe("parseExitCode", () => {
  it.each([
    ["Exited with status 75", 75],
    ["exit code: 2", 2],
    ["exited with code 64", 64],
    ["container exit status 137", 137],
    ["exit code=3", 3],
    ["exit 1", 1],
    ["exited\twith code 9", 9],
    ["no number here", undefined],
    ["", undefined],
    [null, undefined],
    [undefined, undefined],
  ] as const)("%j → %j", (text, want) => {
    expect(parseExitCode(text)).toBe(want);
  });
});

describe("paginateCursor", () => {
  it("follows cursors until one is empty", async () => {
    const pages: Record<string, { items: number[]; cursor?: string | null }> = {
      "": { items: [1, 2], cursor: "b" },
      b: { items: [3], cursor: "c" },
      c: { items: [], cursor: null },
    };
    const seen: (string | undefined)[] = [];
    const out: number[] = [];
    for await (const n of paginateCursor(async (c) => {
      seen.push(c);
      return pages[c ?? ""]!;
    }))
      out.push(n);
    expect(out).toEqual([1, 2, 3]);
    expect(seen).toEqual([undefined, "b", "c"]);
  });

  it("stops after maxPages", async () => {
    let calls = 0;
    const out: number[] = [];
    for await (const n of paginateCursor(async () => ({ items: [++calls], cursor: "same" }), 3)) out.push(n);
    expect(out).toEqual([1, 2, 3]);
  });
});

describe("ProviderHttpClient", () => {
  interface Call {
    url: string;
    init: RequestInit;
  }
  function client(
    respond: (url: string) => Response | Promise<Response>,
    extra: { retryAfterHeaders?: string[] } = {},
  ) {
    const calls: Call[] = [];
    const errors: HttpErrorInfo[] = [];
    const c = new ProviderHttpClient({
      provider: "fake",
      baseUrl: "https://api.example.test/v1/",
      apiKey: "test-key",
      headers: { "x-org": "o1" },
      fetch: (async (url: string, init: RequestInit) => {
        calls.push({ url, init });
        return respond(url);
      }) as typeof fetch,
      toError: (e, what) => {
        errors.push(e);
        return new ProviderError(
          e.status === 429 ? "rate_limited" : "unavailable",
          `fake ${what}: ${e.message}`,
          e.retryAfter,
          e.code || undefined,
        );
      },
      ...extra,
    });
    return { c, calls, errors };
  }

  it("sends the Bearer token, default headers and a JSON body", async () => {
    const { c, calls } = client(() => Response.json({ id: "x" }));
    const res = await c.call("POST", "/things", { a: 1 }, { "idempotency-key": "k" });
    expect(await c.json(res, "create")).toEqual({ id: "x" });
    expect(calls[0]!.url).toBe("https://api.example.test/v1/things");
    expect(calls[0]!.init.method).toBe("POST");
    expect(calls[0]!.init.body).toBe('{"a":1}');
    expect(calls[0]!.init.headers).toEqual({
      authorization: "Bearer test-key",
      accept: "application/json",
      "x-org": "o1",
      "idempotency-key": "k",
      "content-type": "application/json",
    });
  });

  it("takes an absolute URL as is and sends no body without one", async () => {
    const { c, calls } = client(() => new Response(null, { status: 204 }));
    await c.call("GET", "https://toolbox.example.test/s/1");
    expect(calls[0]!.url).toBe("https://toolbox.example.test/s/1");
    expect(calls[0]!.init.body).toBeUndefined();
    expect(calls[0]!.init.headers).not.toHaveProperty("content-type");
  });

  it("maps a network failure to unavailable without the query string", async () => {
    const { c } = client(() => {
      throw new TypeError("connection reset");
    });
    const err = await c.call("GET", "/things?label=secret").catch((e: unknown) => e);
    expect(parseProviderError(err)).toMatchObject({
      kind: "unavailable",
      detail: "fake GET /v1/things: connection reset",
    });
  });

  it.each([
    ["error object", { error: { code: "limit_reached", message: "too many" } }, "limit_reached", "too many"],
    ["message array", { message: ["a", "b"], error: "Bad Request" }, "", "a; b"],
    ["message string", { code: "c1", message: "m1" }, "c1", "m1"],
    ["error string", { error: "Conflict" }, "", "Conflict"],
    ["code only", { code: "c2" }, "c2", "c2"],
  ])("reads the error body: %s", async (_name, body, code, message) => {
    const { c, errors } = client(() => Response.json(body, { status: 400, statusText: "Bad Request" }));
    await expect(c.json(await c.call("GET", "/x"), "get")).rejects.toBeInstanceOf(ProviderError);
    expect(errors[0]).toEqual({ status: 400, code, message });
  });

  it("falls back to the status text for a body that is not JSON", async () => {
    const { c } = client(() => new Response("oops", { status: 502, statusText: "Bad Gateway" }));
    expect(await c.readError(await c.call("GET", "/x"))).toEqual({ status: 502, code: "", message: "Bad Gateway" });
  });

  it("reads the retry delay from the first retry header that parses", async () => {
    const { c } = client(
      () => Response.json({}, { status: 429, headers: { "retry-after-special": "nope", "retry-after": "7" } }),
      { retryAfterHeaders: ["retry-after-special", "retry-after"] },
    );
    const err = await c.json(await c.call("POST", "/x"), "start").catch((e: unknown) => e);
    expect(err).toMatchObject({ kind: "rate_limited", retryAfterSeconds: 7 });
  });
});
