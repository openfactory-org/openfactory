import { OfError } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import { MAX_WEBHOOK_BODY, constantTimeEqual, readBody, setCapped } from "./util";

describe("constantTimeEqual", () => {
  it.each([
    { a: "secret", b: "secret", want: true },
    { a: "secret", b: "secreT", want: false },
    { a: "secret", b: "secret2", want: false },
    { a: "secret2", b: "secret", want: false },
    { a: "secret", b: "secret\0", want: false },
    { a: "", b: "", want: false },
    { a: "x", b: "", want: false },
    { a: "é", b: "é", want: true },
  ])("$a vs $b is $want", ({ a, b, want }) => {
    expect(constantTimeEqual(a, b)).toBe(want);
  });
});

describe("readBody", () => {
  it("reads a small body", async () => {
    expect(await readBody(new Request("https://x.example.test/", { method: "POST", body: "hi" }))).toBe("hi");
  });

  it("rejects a body over the cap with an OfError", async () => {
    let sent = 0;
    const body = new ReadableStream<Uint8Array>({
      pull(c) {
        if (sent > MAX_WEBHOOK_BODY) return c.close();
        const chunk = new Uint8Array(1 << 20);
        sent += chunk.length;
        c.enqueue(chunk);
      },
    });
    const req = new Request("https://x.example.test/", { method: "POST", body, duplex: "half" } as RequestInit);
    const err = await readBody(req).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OfError);
    expect((err as Error).message).toContain("request body too large");
  });
});

describe("setCapped", () => {
  it("keeps at most max entries and drops the oldest", () => {
    const m = new Map<string, number>();
    for (let i = 0; i < 5; i++) setCapped(m, `k${i}`, i, 3);
    expect([...m.keys()]).toEqual(["k2", "k3", "k4"]);
    setCapped(m, "k2", 9, 3);
    expect([...m.entries()]).toEqual([
      ["k3", 3],
      ["k4", 4],
      ["k2", 9],
    ]);
  });
});
