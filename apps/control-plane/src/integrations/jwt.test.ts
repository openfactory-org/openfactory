import { describe, expect, it } from "vitest";
import { b64urlEncode, parseJwt, signHS256 } from "./jwt";

const part = (v: string) => b64urlEncode(v);

describe("parseJwt", () => {
  it("parses a signed token", async () => {
    const tok = await signHS256("k", { sub: "u1", exp: 1 });
    const j = parseJwt(tok);
    expect(j.claimString("sub")).toBe("u1");
    await expect(j.verifyHS256("k")).resolves.toBeUndefined();
  });

  it.each([
    { name: "null header", tok: `${part("null")}.${part("{}")}.sig` },
    { name: "array header", tok: `${part("[]")}.${part("{}")}.sig` },
    { name: "null claims", tok: `${part('{"alg":"HS256"}')}.${part("null")}.sig` },
    { name: "number claims", tok: `${part('{"alg":"HS256"}')}.${part("42")}.sig` },
    { name: "string claims", tok: `${part('{"alg":"HS256"}')}.${part('"x"')}.sig` },
    { name: "two parts", tok: `${part("{}")}.${part("{}")}` },
  ])("rejects $name", ({ tok }) => {
    expect(() => parseJwt(tok)).toThrow(/^jwt: /);
  });
});
