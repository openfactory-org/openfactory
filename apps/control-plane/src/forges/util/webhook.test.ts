import { describe, expect, it } from "vitest";
import { OfError } from "@openfactory/core";
import { readBody } from "./webhook";

/** A request whose body has `chunks` chunks of `size` bytes and no Content-Length; counts the chunks read. */
function streamed(chunks: number, size: number): { req: Request; pulled: () => number } {
  let n = 0;
  const body = new ReadableStream<Uint8Array>({
    pull(c) {
      if (n >= chunks) return c.close();
      n++;
      c.enqueue(new Uint8Array(size).fill(97));
    },
  });
  const req = new Request("https://forge.example.test/hook", { method: "POST", body, duplex: "half" } as RequestInit);
  return { req, pulled: () => n };
}

describe("readBody", () => {
  it("returns a body within the limit", async () => {
    const { req } = streamed(2, 4);
    expect(new TextDecoder().decode(await readBody(req, 8))).toBe("aaaaaaaa");
  });

  it("stops reading a body without Content-Length at the limit", async () => {
    const { req, pulled } = streamed(1000, 1024);
    const err = await readBody(req, 2048).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OfError);
    expect((err as OfError).code).toBe("invalid_request");
    expect(pulled()).toBeLessThan(10);
  });

  it("rejects a declared Content-Length above the limit before the read", async () => {
    const req = new Request("https://forge.example.test/hook", {
      method: "POST",
      body: "x".repeat(16),
      headers: { "content-length": "16" },
    });
    await expect(readBody(req, 8)).rejects.toThrow(/too large/);
  });
});
