import { describe, expect, it } from "vitest";
import { ApiClient, ApiError, isNotFound, redactUrl, statusOf } from "../src/http";
import { fromApiError, isNotSupported, notSupported, rpc, toPluginError } from "../src/errors";
import { mockFetch } from "../src/testing";

const client = (routes: Parameters<typeof mockFetch>[0]) => {
  const m = mockFetch(routes);
  return {
    api: new ApiClient({
      baseUrl: "https://x.test/base/",
      userAgent: "ua",
      fetch: m.fetch,
      authorize: (h) => h.set("X-Auth", "1"),
    }),
    calls: m.calls,
  };
};

describe("ApiClient", () => {
  it("sends JSON, auth and user agent and joins paths", async () => {
    const { api, calls } = client({ "POST /base/a": () => ({ ok: true }) });
    expect(await api.json("POST", "/a", { n: 1 })).toEqual({ ok: true });
    expect(calls[0]!.headers.get("X-Auth")).toBe("1");
    expect(calls[0]!.headers.get("User-Agent")).toBe("ua");
    expect(calls[0]!.headers.get("Content-Type")).toBe("application/json");
    expect(calls[0]!.body).toEqual({ n: 1 });
  });

  it("sends form bodies and raw JSON strings", async () => {
    const { api, calls } = client({ "POST /base/f": () => ({}) });
    await api.send("POST", "f", new URLSearchParams({ a: "1" }));
    expect(calls[0]!.headers.get("Content-Type")).toBe("application/x-www-form-urlencoded");
    expect(calls[0]!.text).toBe("a=1");
    await api.send("POST", "f", '{"raw":true}');
    expect(calls[1]!.body).toEqual({ raw: true });
  });

  it("returns text, bytes and nothing for an empty body", async () => {
    const { api } = client({ "GET /base/t": () => "plain", "GET /base/e": () => new Response("  \n") });
    expect(await api.text("GET", "t")).toBe("plain");
    expect(new TextDecoder().decode(await api.bytes("GET", "t"))).toBe("plain");
    expect(await api.json("GET", "e")).toBeUndefined();
  });

  it("throws ApiError with status, redacted URL and a body excerpt", async () => {
    const fake = (async () => new Response("slow down", { status: 429 })) as unknown as typeof fetch;
    const api = new ApiClient({ baseUrl: "https://u:p@x.test", fetch: fake });
    const e = await api.json("GET", "/x").catch((x: unknown) => x);
    expect(e).toBeInstanceOf(ApiError);
    const ae = e as ApiError;
    expect(ae.status).toBe(429);
    expect(ae.temporary).toBe(true);
    expect(ae.message).toContain("slow down");
    expect(ae.message).not.toContain("u:p");
    expect(statusOf(e)).toBe(429);
    expect(isNotFound(new ApiError("GET", "u", 404, ""))).toBe(true);
    expect(statusOf(new Error("x"))).toBe(0);
  });

  it("fails on a body that is not JSON", async () => {
    const { api } = client({ "GET /base/bad": () => new Response("<html>") });
    await expect(api.json("GET", "bad")).rejects.toThrow(/decode GET/);
  });

  it("redacts credentials of a URL", () => {
    expect(redactUrl("https://u:p@h.test/a?b=1")).toBe("https://h.test/a?b=1");
    expect(redactUrl("not a url")).toBe("not a url");
  });
});

describe("errors", () => {
  it.each([
    [401, "external_authentication_required"],
    [403, "not_authorized"],
    [404, "resource_not_found"],
    [409, "conflict"],
    [400, "invalid_request"],
    [422, "invalid_request"],
    [429, "resource_unavailable"],
    [502, "resource_unavailable"],
    [418, "internal_error"],
  ])("maps HTTP %i to %s", (status, code) => {
    expect(fromApiError(new ApiError("GET", "u", status, "b")).code).toBe(code);
  });

  it("marks an unsupported operation and recognizes it after RPC", () => {
    const e = notSupported("review vote");
    expect(e.message).toBe("operation_not_supported: review vote");
    expect(isNotSupported(e)).toBe(true);
    // Across RPC the error is a plain Error with the same message.
    expect(isNotSupported(new Error(e.message))).toBe(true);
    expect(isNotSupported(new Error("boom"))).toBe(false);
  });

  it("maps unknown failures to internal_error and keeps known ones", async () => {
    expect(toPluginError(new Error("boom")).code).toBe("internal_error");
    expect(toPluginError("text").code).toBe("internal_error");
    expect(toPluginError(new Error("conflict: x")).code).toBe("conflict");
    await expect(rpc(() => Promise.reject(new Error("boom")))).rejects.toMatchObject({ code: "internal_error" });
    expect(await rpc(() => 5)).toBe(5);
  });
});
