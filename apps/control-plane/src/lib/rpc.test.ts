import { describe, expect, it } from "vitest";
import { OfError } from "@openfactory/core";
import { isOpError, isOpResponse, unwrapOp } from "./rpc";

function thrown(f: () => unknown): OfError {
  try {
    f();
  } catch (e) {
    if (e instanceof OfError) return e;
    throw e;
  }
  throw new Error("no error thrown");
}

describe("unwrapOp", () => {
  const values: { name: string; out: unknown; want: unknown }[] = [
    { name: "plain object", out: { id: "x" }, want: { id: "x" } },
    { name: "null", out: null, want: null },
    { name: "undefined", out: undefined, want: undefined },
    { name: "string", out: "s", want: "s" },
    { name: "OpResponse body", out: { $status: 201, body: { id: "y" } }, want: { id: "y" } },
    { name: "OpResponse without body", out: { $status: 204 }, want: undefined },
    { name: "null $error is not an error", out: { $error: null, a: 1 }, want: { $error: null, a: 1 } },
  ];
  for (const c of values) {
    it(`returns the value: ${c.name}`, () => {
      expect(unwrapOp(c.out)).toEqual(c.want);
    });
  }

  it("throws every OfError field of an OpError", () => {
    const e = thrown(() =>
      unwrapOp({
        $error: {
          code: "external_authentication_required",
          detail: "link your account",
          reason: "no_linked_account",
          provider: "github",
          authUrl: "https://example.com/auth",
          setupUrl: "https://example.com/setup",
          extra: { errors: ["a"] },
        },
      }),
    );
    expect(e.code).toBe("external_authentication_required");
    expect(e.detail).toBe("link your account");
    expect(e.reason).toBe("no_linked_account");
    expect(e.provider).toBe("github");
    expect(e.authUrl).toBe("https://example.com/auth");
    expect(e.setupUrl).toBe("https://example.com/setup");
    expect(e.extra).toEqual({ errors: ["a"] });
  });

  const errors: { name: string; err: { code: string; detail: string }; op?: string; code: string; detail: string }[] = [
    { name: "known code", err: { code: "conflict", detail: "busy" }, code: "conflict", detail: "busy" },
    {
      name: "op prefix",
      err: { code: "resource_not_found", detail: "user u not found" },
      op: "getUser",
      code: "resource_not_found",
      detail: "getUser: user u not found",
    },
    { name: "unknown code", err: { code: "bogus", detail: "x" }, code: "internal_error", detail: "x" },
  ];
  for (const c of errors) {
    it(`maps the error: ${c.name}`, () => {
      const e = thrown(() => unwrapOp({ $error: c.err }, c.op));
      expect(e.code).toBe(c.code);
      expect(e.detail).toBe(c.detail);
    });
  }
});

describe("isOpError and isOpResponse", () => {
  it("tell the envelopes apart", () => {
    expect(isOpError({ $error: { code: "conflict", detail: "" } })).toBe(true);
    expect(isOpError({ $status: 201 })).toBe(false);
    expect(isOpError(null)).toBe(false);
    expect(isOpResponse({ $status: 201 })).toBe(true);
    expect(isOpResponse({ $status: "201" })).toBe(false);
    expect(isOpResponse({ $error: { code: "conflict", detail: "" } })).toBe(false);
  });
});
