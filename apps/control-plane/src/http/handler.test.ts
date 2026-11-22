import { describe, expect, it } from "vitest";
import { OfError } from "@openfactory/core";
import { json, opResult, readJson } from "./handler";

const post = (body: string, headers: Record<string, string> = {}) =>
  new Request("http://localhost/x", { method: "POST", body, headers });

async function rejection(p: Promise<unknown>): Promise<OfError> {
  try {
    await p;
  } catch (e) {
    if (e instanceof OfError) return e;
    throw e;
  }
  throw new Error("no error thrown");
}

describe("readJson", () => {
  it("counts the limit in UTF-8 bytes, not UTF-16 code units", async () => {
    // 6 UTF-16 code units, 14 bytes: under a 10-unit limit, over a 10-byte limit.
    const body = JSON.stringify("€€€€");
    expect(body.length).toBeLessThanOrEqual(10);
    expect(new TextEncoder().encode(body).length).toBeGreaterThan(10);
    const e = await rejection(readJson(post(body), true, 10));
    expect(e.code).toBe("invalid_request");
    expect(e.detail).toBe("request body exceeds 10 bytes");
  });

  it("accepts a body of exactly max bytes", async () => {
    const body = JSON.stringify("€"); // 5 bytes
    expect(await readJson(post(body), true, 5)).toBe("€");
  });

  it("rejects a Content-Length above the limit", async () => {
    const e = await rejection(readJson(post("{}", { "content-length": "999" }), true, 10));
    expect(e.detail).toBe("request body exceeds 10 bytes");
  });

  const cases: { name: string; body: string; optional: boolean; want?: unknown; detail?: string }[] = [
    { name: "empty optional", body: "  ", optional: true, want: undefined },
    { name: "empty required", body: "", optional: false, detail: "request body is required" },
    { name: "object", body: '{"a":1}', optional: true, want: { a: 1 } },
    { name: "malformed", body: "{", optional: true, detail: "malformed JSON body:" },
  ];
  for (const c of cases) {
    it(c.name, async () => {
      if (c.detail === undefined) {
        expect(await readJson(post(c.body), c.optional)).toEqual(c.want);
        return;
      }
      const e = await rejection(readJson(post(c.body), c.optional));
      expect(e.code).toBe("invalid_request");
      expect(e.detail.startsWith(c.detail)).toBe(true);
    });
  }
});

describe("json", () => {
  it("takes the body first and the status second", async () => {
    const r = json({ a: 1 }, 201);
    expect(r.status).toBe(201);
    expect(r.headers.get("content-type")).toBe("application/json");
    expect(await r.text()).toBe('{"a":1}\n');
  });

  it("sends no body with 204", async () => {
    const r = json({ a: 1 }, 204);
    expect(r.status).toBe(204);
    expect(await r.text()).toBe("");
  });
});

describe("opResult", () => {
  it("maps an OpResponse to its status, body and headers", async () => {
    const r = opResult({ $status: 201, body: { id: "x" }, headers: { location: "/x" } });
    expect(r.status).toBe(201);
    expect(r.headers.get("location")).toBe("/x");
    expect(await r.json()).toEqual({ id: "x" });
  });

  it("maps a plain value to 200", async () => {
    const r = opResult({ id: "y" });
    expect(r.status).toBe(200);
    expect(await r.json()).toEqual({ id: "y" });
  });

  it("throws the OfError of an OpError with reason and extra", () => {
    let err: unknown;
    try {
      opResult({ $error: { code: "invalid_request", detail: "bad", reason: "r", extra: { errors: [1] } } });
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(OfError);
    expect((err as OfError).reason).toBe("r");
    expect((err as OfError).extra).toEqual({ errors: [1] });
  });
});
