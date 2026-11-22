import { describe, expect, it } from "vitest";
import {
  API_MAX_ERROR_BODY_BYTES,
  API_MAX_RESPONSE_BYTES,
  ApiClient,
  ApiError,
  OfError,
  isNotFound,
  statusCode,
  type AuthorizeRequest,
} from "../src/index";

/** A response body of total bytes, sent in 1 MiB chunks of "a". */
function big(total: number): ReadableStream<Uint8Array> {
  const chunk = new Uint8Array(1 << 20).fill(0x61);
  let sent = 0;
  return new ReadableStream({
    pull(c) {
      if (sent >= total) return c.close();
      const n = Math.min(chunk.length, total - sent);
      sent += n;
      c.enqueue(n === chunk.length ? chunk : chunk.subarray(0, n));
    },
  });
}

function client(resp: () => Response): ApiClient {
  return new ApiClient({ baseUrl: "https://api.example.test", fetch: async () => resp() });
}

describe("ApiClient body caps", () => {
  it("reads a normal JSON response", async () => {
    expect(await client(() => Response.json({ ok: true })).json("GET", "x")).toEqual({ ok: true });
  });

  it("rejects a 2xx response over the cap instead of truncating it", async () => {
    const err = await client(() => new Response(big(API_MAX_RESPONSE_BYTES + 1)))
      .raw("GET", "x")
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OfError);
    expect((err as OfError).code).toBe("resource_unavailable");
    expect((err as Error).message).toContain("response body too large");
  });

  it("keeps the status of a large error response and cuts its body", async () => {
    const err = await client(() => new Response(big(API_MAX_ERROR_BODY_BYTES * 4), { status: 404 }))
      .raw("GET", "x")
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(404);
    expect((err as ApiError).body.length).toBe(API_MAX_ERROR_BODY_BYTES);
  });
});

describe("ApiClient requests", () => {
  it("passes the method, URL and credential to authorize", async () => {
    const seen: AuthorizeRequest[] = [];
    const sent: Request[] = [];
    const api = new ApiClient({
      baseUrl: "https://api.example.test/v1/",
      headers: { "X-Extra": "1" },
      fetch: async (i, init) => {
        sent.push(new Request(i, init));
        return new Response(null, { status: 204 });
      },
      authorize: (h, req) => {
        seen.push(req);
        h.set("Authorization", req.auth ?? "none");
      },
    });
    await api.send("POST", "/a?b=1", { n: 1 }, "Bearer t");
    await api.send("GET", "https://other.test/x");
    expect(seen.map((r) => [r.method, r.url.toString(), r.auth])).toEqual([
      ["POST", "https://api.example.test/v1/a?b=1", "Bearer t"],
      ["GET", "https://other.test/x", undefined],
    ]);
    expect(sent[0]!.headers.get("Authorization")).toBe("Bearer t");
    expect(sent[0]!.headers.get("X-Extra")).toBe("1");
    expect(sent[0]!.headers.get("Content-Type")).toBe("application/json");
    expect(sent[1]!.headers.get("Authorization")).toBe("none");
  });

  it("returns the status and headers of a raw answer", async () => {
    const r = await client(() => new Response("hi", { status: 201, headers: { Link: "<x>" } })).raw("GET", "x");
    expect([r.status, r.text, r.headers.get("Link")]).toEqual([201, "hi", "<x>"]);
  });
});

describe("ApiError", () => {
  it.each([
    [429, true],
    [500, true],
    [503, true],
    [404, false],
    [400, false],
  ])("HTTP %i is temporary: %s", (status, want) => {
    expect(new ApiError("GET", "u", status, "").temporary).toBe(want);
  });

  it("finds the status through cause chains", () => {
    const inner = new ApiError("GET", "u", 404, "");
    const wrapped = new Error("outer", { cause: new Error("middle", { cause: inner }) });
    expect(statusCode(inner)).toBe(404);
    expect(statusCode(wrapped)).toBe(404);
    expect(isNotFound(wrapped)).toBe(true);
    expect(statusCode(new Error("x"))).toBe(0);
    expect(statusCode(undefined)).toBe(0);
  });
});
