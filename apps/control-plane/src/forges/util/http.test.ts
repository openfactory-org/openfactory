import { OfError } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import { ApiError, HttpClient, MAX_ERROR_BODY_BYTES, MAX_RESPONSE_BYTES } from "./http";

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

function client(resp: () => Response): HttpClient {
  return new HttpClient({ baseURL: "https://api.example.test", fetch: async () => resp() });
}

describe("HttpClient body caps", () => {
  it("reads a normal JSON response", async () => {
    expect(await client(() => Response.json({ ok: true })).do("GET", "x")).toEqual({ ok: true });
  });

  it("rejects a 2xx response over the cap instead of truncating it", async () => {
    const err = await client(() => new Response(big(MAX_RESPONSE_BYTES + 1)))
      .raw("GET", "x")
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OfError);
    expect((err as OfError).code).toBe("resource_unavailable");
    expect((err as Error).message).toContain("response body too large");
  });

  it("keeps the status of a large error response and cuts its body", async () => {
    const err = await client(() => new Response(big(MAX_ERROR_BODY_BYTES * 4), { status: 404 }))
      .raw("GET", "x")
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(404);
    expect((err as ApiError).body.length).toBe(MAX_ERROR_BODY_BYTES);
  });
});
