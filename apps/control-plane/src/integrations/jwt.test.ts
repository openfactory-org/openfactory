import { b64url, utf8 } from "@openfactory/core";
import { SignJWT } from "jose";
import { describe, expect, it } from "vitest";
import { decodeUnverified, importRsaJwk, verifyJwt, type VerifyJwtOptions } from "./jwt";

const part = (v: string) => b64url(utf8(v));
const NOW_S = 1_700_000_000;
const hs = (claims: Record<string, unknown>, secret = "k") =>
  new SignJWT(claims).setProtectedHeader({ alg: "HS256", typ: "JWT" }).sign(utf8(secret));
const opts = (o: Partial<VerifyJwtOptions> = {}): VerifyJwtOptions => ({
  alg: "HS256",
  nowMs: NOW_S * 1000,
  skewMs: 60_000,
  ...o,
});

describe("decodeUnverified", () => {
  it("reads the header and claims", async () => {
    const d = decodeUnverified(await hs({ sub: "u1", exp: 1 }));
    expect(d.header).toMatchObject({ alg: "HS256" });
    expect(d.claims).toMatchObject({ sub: "u1" });
  });

  it.each([
    { name: "null header", tok: `${part("null")}.${part("{}")}.sig` },
    { name: "array header", tok: `${part("[]")}.${part("{}")}.sig` },
    { name: "null claims", tok: `${part('{"alg":"HS256"}')}.${part("null")}.sig` },
    { name: "number claims", tok: `${part('{"alg":"HS256"}')}.${part("42")}.sig` },
    { name: "string claims", tok: `${part('{"alg":"HS256"}')}.${part('"x"')}.sig` },
    { name: "two parts", tok: `${part("{}")}.${part("{}")}` },
  ])("rejects $name", ({ tok }) => {
    expect(() => decodeUnverified(tok)).toThrow(/^jwt: /);
  });
});

describe("verifyJwt", () => {
  it("returns the claims of a valid token", async () => {
    const claims = await verifyJwt(await hs({ sub: "u1", exp: NOW_S + 10 }), "k", opts());
    expect(claims).toMatchObject({ sub: "u1" });
  });

  it.each([
    { name: "wrong secret", claims: { exp: NOW_S + 10 }, secret: "x", o: {}, err: /^jwt: invalid signature$/ },
    {
      name: "other alg",
      claims: { exp: NOW_S + 10 },
      o: { alg: "RS256" as const },
      err: /^jwt: unexpected alg "HS256"$/,
    },
    { name: "missing exp", claims: {}, o: {}, err: /^jwt: missing exp$/ },
    { name: "exp not a number", claims: { exp: "soon" }, o: {}, err: /^jwt: missing exp$/ },
    { name: "expired past the skew", claims: { exp: NOW_S - 61 }, o: {}, err: /^jwt: token expired$/ },
    {
      name: "nbf past the skew",
      claims: { exp: NOW_S + 600, nbf: NOW_S + 61 },
      o: {},
      err: /^jwt: token not yet valid$/,
    },
    { name: "wrong issuer", claims: { exp: NOW_S + 10, iss: "a" }, o: { issuer: "b" }, err: /unexpected issuer "a"/ },
    { name: "no issuer", claims: { exp: NOW_S + 10 }, o: { issuer: "b" }, err: /unexpected issuer|iss/ },
    { name: "wrong audience", claims: { exp: NOW_S + 10, aud: "a" }, o: { audience: ["b"] }, err: /audience/ },
    { name: "no audience", claims: { exp: NOW_S + 10 }, o: { audience: ["b"] }, err: /audience/ },
    { name: "empty audience list", claims: { exp: NOW_S + 10, aud: "a" }, o: { audience: [] }, err: /audience/ },
  ])("rejects $name", async ({ claims, secret, o, err }) => {
    await expect(verifyJwt(await hs(claims, secret), "k", opts(o))).rejects.toThrow(err);
  });

  it.each([
    { name: "expired within the skew", claims: { exp: NOW_S - 30 }, o: {} },
    { name: "nbf within the skew", claims: { exp: NOW_S + 600, nbf: NOW_S + 30 }, o: {} },
    { name: "matching issuer", claims: { exp: NOW_S + 10, iss: "a" }, o: { issuer: "a" } },
    { name: "one of the audiences", claims: { exp: NOW_S + 10, aud: ["x", "b"] }, o: { audience: ["a", "b"] } },
  ])("accepts $name", async ({ claims, o }) => {
    await expect(verifyJwt(await hs(claims), "k", opts(o))).resolves.toBeDefined();
  });

  it("verifies RS256 with an imported JWK", async () => {
    const pair = (await crypto.subtle.generateKey(
      { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
      true,
      ["sign", "verify"],
    )) as CryptoKeyPair;
    const jwk = (await crypto.subtle.exportKey("jwk", pair.publicKey)) as JsonWebKey;
    const pub = await importRsaJwk({ kid: "k1", n: jwk.n!, e: jwk.e! });
    const tok = await new SignJWT({ exp: NOW_S + 10 }).setProtectedHeader({ alg: "RS256" }).sign(pair.privateKey);
    await expect(verifyJwt(tok, pub, opts({ alg: "RS256" }))).resolves.toBeDefined();
    await expect(importRsaJwk({ kty: "EC", kid: "k2", n: "", e: "" })).rejects.toThrow(/unsupported kty/);
  });
});
