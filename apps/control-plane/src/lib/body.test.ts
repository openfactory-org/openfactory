import { OfError } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import {
  bodyObject,
  optBool,
  optNullableStringMap,
  optObject,
  optString,
  optStringArray,
  optStringMap,
  readBytesCapped,
  readTextCapped,
  readTextPrefix,
} from "./body";

/** A body stream of n chunks of size bytes each, with no Content-Length. */
function chunked(n: number, size: number): ReadableStream<Uint8Array> {
  let left = n;
  return new ReadableStream({
    pull(c) {
      if (left-- <= 0) return c.close();
      c.enqueue(new Uint8Array(size).fill(0x61));
    },
  });
}

describe("readBytesCapped", () => {
  it.each([
    { name: "empty", body: null, max: 4, want: "" },
    { name: "below the cap", body: "abc", max: 4, want: "abc" },
    { name: "at the cap", body: "abcd", max: 4, want: "abcd" },
  ])("reads $name", async ({ body, max, want }) => {
    expect(await readTextCapped(new Response(body), max)).toBe(want);
  });

  it("rejects a streamed body longer than the cap", async () => {
    const err = await readBytesCapped(new Response(chunked(5, 3)), 10, "request body").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OfError);
    expect((err as OfError).code).toBe("invalid_request");
    expect((err as OfError).message).toContain("request body too large: more than 10 bytes");
  });

  it("rejects on Content-Length before the read", async () => {
    const req = new Request("https://x.example.test/", {
      method: "POST",
      body: "abcdef",
      headers: { "content-length": "6" },
    });
    await expect(readBytesCapped(req, 5)).rejects.toThrow(/too large/);
  });

  it("uses the given error code", async () => {
    const err = await readBytesCapped(new Response("abcdef"), 2, "x", "resource_unavailable").catch((e: unknown) => e);
    expect((err as OfError).code).toBe("resource_unavailable");
  });
});

describe("readTextPrefix", () => {
  it("keeps the first max bytes and drops the rest", async () => {
    expect(await readTextPrefix(new Response(chunked(4, 3)), 7)).toBe("aaaaaaa");
    expect(await readTextPrefix(new Response("ab"), 7)).toBe("ab");
  });
});

describe("field type errors", () => {
  it.each([
    { name: "string", fn: () => optString({ f: 1 }, "f"), want: 'field "f" must be a string' },
    { name: "boolean", fn: () => optBool({ f: "yes" }, "f"), want: 'field "f" must be a boolean' },
    { name: "string array", fn: () => optStringArray({ f: [1] }, "f"), want: 'field "f" must be an array of strings' },
    {
      name: "string map",
      fn: () => optStringMap({ f: { a: 1 } }, "f"),
      want: 'field "f" must be an object with string values',
    },
    {
      name: "nullable string map",
      fn: () => optNullableStringMap({ f: { a: 1 } }, "f"),
      want: 'field "f" must be an object with string or null values',
    },
    { name: "object", fn: () => optObject({ f: [] }, "f"), want: 'field "f" must be an object' },
    { name: "body", fn: () => bodyObject("x"), want: "malformed JSON body: expected an object" },
  ])("names the JSON type: $name", ({ fn, want }) => {
    const err = (() => {
      try {
        fn();
      } catch (e) {
        return e;
      }
      return undefined;
    })();
    expect(err).toBeInstanceOf(OfError);
    expect((err as OfError).code).toBe("invalid_request");
    expect((err as OfError).detail).toBe(want);
  });

  it("treats null as an absent field", () => {
    expect(optString({ f: null }, "f")).toBeUndefined();
    expect(optObject({ f: null }, "f")).toBeUndefined();
    expect(optObject({ f: { a: 1 } }, "f")).toEqual({ a: 1 });
    expect(bodyObject(undefined, true)).toEqual({});
  });
});
