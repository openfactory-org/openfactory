// Ports of internal/auth (seal, roles, OIDC identity mapping), internal/api
// auth (key format) and internal/runnertoken. Goldens come from a Go program
// that runs the go-final code paths (seal.go, runnertoken/session.go).
import { describe, expect, it } from "vitest";
import type { Principal } from "../contracts/do-messages";
import { generateKey, parseKey } from "./apikeys";
import {
  OIDC_MAX_RESPONSE_BYTES,
  claimStrings,
  discover,
  identityFromClaims,
  mapRole,
  oidcConfigFromEnv,
  safeReturnTo,
  type OIDCConfig,
} from "./oidc";
import { actionForRole, canOn, isFactoryScoped, requireRole, roleAuthorizerAllow, roleFor } from "./roles";
import { hashBootstrap, mintSessionToken, staticKeys, verifySessionToken } from "./run";
import { Sealer } from "./seal";

const enc = new TextEncoder();
const MASTER = enc.encode("test-only-fake-signing-key-32byt");
const OTHER = enc.encode("test-only-other-signing-key-32by");

describe("seal", () => {
  it("opens a cookie sealed by Go (seal.go)", async () => {
    const goSealed =
      "gzBhvMXMqBnMvtFUnqxTnUdxNpPDmRbvkroM4V3xa3UsAG0IQJfVrPKbE9Kx_noCMPUCzgeZYfWALP4KZ1mmn8GkZrj4DKH04-FCRScIg96n17oGBnLU57Iw3_LQDrsHahjoIg";
    const s = await Sealer.create(MASTER, "session");
    expect(await s.open(goSealed, "of_session")).toEqual({ u: "usr_1", t: "default", p: "api_key", iat: 1 });
    expect(await s.open(goSealed, "of_flow_oidc")).toBeNull();
    expect(await (await Sealer.create(MASTER, "login-flow")).open(goSealed, "of_session")).toBeNull();
  });

  it("round-trips, expires, rejects tampering, and opens with previous keys only", async () => {
    const s = await Sealer.create(MASTER, "session");
    const tok = await s.seal({ a: 1 }, "c", Date.now() + 60_000);
    expect(await s.open(tok, "c")).toEqual({ a: 1 });
    expect(await s.open(tok, "c", Date.now() + 61_000)).toBeNull();
    expect(await s.open(tok.slice(0, -2) + (tok.endsWith("A") ? "BB" : "AA"), "c")).toBeNull();
    expect(await s.open("!!", "c")).toBeNull();
    // seal_rotation_test: a rotated sealer opens old cookies; the old one cannot open new ones.
    const rotated = await Sealer.create(OTHER, "session", [MASTER]);
    expect(await rotated.open(tok, "c")).toEqual({ a: 1 });
    expect(await s.open(await rotated.seal({ b: 2 }, "c", Date.now() + 60_000), "c")).toBeNull();
    await expect(Sealer.create(enc.encode("short"), "session")).rejects.toThrow(/32 bytes/);
  });
});

describe("API keys", () => {
  it("generates and parses of_ and ofa_ keys", async () => {
    const g = await generateKey();
    expect(g.key).toMatch(/^of_[a-z0-9]{8}_[A-Za-z0-9]{40}$/);
    expect(parseKey(g.key)).toEqual({ prefix: g.prefix, serviceAccount: false });
    expect(g.hash).toMatch(/^[0-9a-f]{64}$/);
    expect(parseKey((await generateKey("ofa_")).key)?.serviceAccount).toBe(true);
    for (const bad of [
      "of_short_x",
      "of_ABCDEFGH_" + "a".repeat(40),
      "of_abcdefgh_" + "a".repeat(39),
      "ofx_abcdefgh_" + "a".repeat(40),
      "of_abcdefgh" + "a".repeat(41),
    ]) {
      expect(parseKey(bad), bad).toBeNull();
    }
  });
});

describe("roles (internal/api roles_test, auth RoleAuthorizer)", () => {
  const p = (
    role: "user" | "editor" | "admin",
    factoryRoles: Record<string, "user" | "editor" | "admin"> = {},
  ): Principal => ({
    kind: "user",
    id: "usr_1",
    teamId: "default",
    role,
    factoryRoles,
    userId: "usr_1",
  });
  it("applies factory overrides and the Go messages", () => {
    const u = p("user", { fac_1: "admin" });
    expect(roleFor(u, "fac_1")).toBe("admin");
    expect(roleFor(u, "fac_2")).toBe("user");
    expect(canOn(u, "fac_1", "admin")).toBe(true);
    expect(canOn(p("admin", { fac_1: "user" }), "fac_1", "editor")).toBe(false);
    expect(() => requireRole(u, "", "editor")).toThrow("this operation requires the editor role; your role is user");
    expect(() => requireRole(u, "fac_2", "editor")).toThrow(
      "this operation requires the editor role; your role for this factory is user",
    );
    expect(() => requireRole(undefined, "", "user")).toThrow(/missing credentials/);
    expect(requireRole(u, "", "public")).toBe(u);
  });
  it("maps routes to actions and categories to roles", () => {
    expect(actionForRole("GET", "user")).toBe("read");
    expect(actionForRole("POST", "user")).toBe("operate");
    expect(actionForRole("POST", "editor")).toBe("write");
    expect(actionForRole("GET", "admin")).toBe("admin");
    expect(() => roleAuthorizerAllow(p("user"), "write:factory.update")).toThrow(
      "write:factory.update requires the editor role; your role is user",
    );
    expect(() => roleAuthorizerAllow(p("admin"), "read", { teamId: "other" })).toThrow(/another team/);
    roleAuthorizerAllow(p("editor"), "write", { factoryId: "fac_1" });
  });
  it("finds factory-scoped routes like routeFactory", () => {
    expect(isFactoryScoped({ path: "/factories/{uid}/agents" })).toBe(true);
    expect(isFactoryScoped({ path: "/runs/{runId}" })).toBe(true);
    expect(isFactoryScoped({ path: "/inbox/{id}" })).toBe(true);
    expect(isFactoryScoped({ path: "/users/{id}" })).toBe(false);
    expect(isFactoryScoped({ path: "/secrets/{name}" })).toBe(false);
  });
});

describe("OIDC (internal/auth oidc tests)", () => {
  it("caps the discovery document", async () => {
    const doc = (issuer: string, pad = 0) =>
      JSON.stringify({
        issuer,
        authorization_endpoint: issuer + "/auth",
        token_endpoint: issuer + "/token",
        jwks_uri: issuer + "/jwks",
        pad: "x".repeat(pad),
      });
    const small = "https://idp-small.example.test";
    const ok = await discover(small, (async () => new Response(doc(small))) as unknown as typeof fetch);
    expect(ok.d.token_endpoint).toBe(small + "/token");
    const big = "https://idp-big.example.test";
    await expect(
      discover(big, (async () => new Response(doc(big, OIDC_MAX_RESPONSE_BYTES))) as unknown as typeof fetch),
    ).rejects.toThrow(/too large/);
  });

  const cfg = (o: Partial<OIDCConfig> = {}): OIDCConfig => ({
    issuer: "https://idp.example.test",
    clientId: "cid",
    clientSecret: "",
    redirectUrl: "https://factory.example.test/auth/oidc/callback",
    scopes: ["openid"],
    allowedDomains: [],
    groupsClaim: "groups",
    adminGroups: ["ops"],
    editorGroups: ["dev"],
    defaultRole: "user",
    displayName: "SSO",
    requireVerifiedEmail: true,
    ...o,
  });
  it("maps groups to roles", () => {
    expect(mapRole(cfg(), ["dev", "ops"])).toBe("admin");
    expect(mapRole(cfg(), ["dev"])).toBe("editor");
    expect(mapRole(cfg(), ["Ops"])).toBe("user");
    expect(claimStrings("a, b c")).toEqual(["a", "b", "c"]);
    expect(claimStrings(["a", 1, ""])).toEqual(["a"]);
  });
  it("enforces allowed domains and verified email", () => {
    const c = cfg({ allowedDomains: ["example.com"] });
    expect(
      identityFromClaims(c, "s1", { email: "a@example.com", email_verified: "true", groups: ["dev"] }),
    ).toMatchObject({
      subject: "s1",
      emailVerified: true,
      role: "editor",
      roleManaged: true,
    });
    expect(() => identityFromClaims(c, "s1", { email: "a@other.com", email_verified: true })).toThrow(
      /domain_not_allowed/,
    );
    expect(() => identityFromClaims(c, "s1", { email: "a@example.com" })).toThrow(/email_not_verified/);
    expect(identityFromClaims(cfg({ requireVerifiedEmail: false }), "s", { email: "x@y.z" }).emailVerified).toBe(true);
  });
  it("reads the configuration and keeps return paths local", () => {
    const env = {
      OF_OIDC_ISSUER_URL: "https://idp.example.test",
      OF_OIDC_CLIENT_ID: "cid",
      OF_PUBLIC_URL: "https://f.example.test/",
    } as never;
    const c = oidcConfigFromEnv(env)!;
    expect(c.redirectUrl).toBe("https://f.example.test/auth/oidc/callback");
    expect(c.scopes).toEqual(["openid", "profile", "email"]);
    expect(oidcConfigFromEnv({ OF_OIDC_ISSUER_URL: "" } as never)).toBeNull();
    expect(() => oidcConfigFromEnv({ OF_OIDC_ISSUER_URL: "https://i" } as never)).toThrow(/CLIENT_ID/);
    expect(safeReturnTo("/runs")).toBe("/runs");
    for (const bad of ["", "https://evil", "//evil", "/\\evil", "/a\nb"]) expect(safeReturnTo(bad)).toBe("/");
  });
});

describe("runner tokens (internal/runnertoken)", () => {
  const key = enc.encode("0123456789abcdef0123456789abcdef");
  const keys = staticKeys("3eb1bd43", { "3eb1bd43": key });
  const GO_TOKEN =
    "ofs_eyJ2IjoxLCJraWQiOiIzZWIxYmQ0MyIsInRlYW0iOiJkZWZhdWx0IiwicnVuIjoicnVuX3giLCJzZXMiOjIsInN1YiI6InJ1bm5lciIsImlhdCI6MTc5NTAwMDAwMCwiZXhwIjoxNzk1MDAzNjAwfQ.yCcQihkk_Kirp5_5260qbhfmMC2IWjBwGudti2DX3BA";
  it("verifies a token minted by Go and mints the same bytes", async () => {
    const c = await verifySessionToken(keys, GO_TOKEN, 1795000100_000);
    expect(c).toMatchObject({ team: "default", run: "run_x", ses: 2, sub: "runner" });
    const mine = await mintSessionToken(keys, { team: "default", run: "run_x", ses: 2, sub: "runner" }, 1795000000_000);
    expect(mine.token).toBe(GO_TOKEN);
  });
  it("refuses expired, early, tampered, unknown-key and malformed tokens", async () => {
    await expect(verifySessionToken(keys, GO_TOKEN, 1795003600_000)).rejects.toMatchObject({ kind: "expired" });
    await expect(verifySessionToken(keys, GO_TOKEN, 1794999000_000)).rejects.toMatchObject({ kind: "not_yet_valid" });
    await expect(verifySessionToken(keys, GO_TOKEN.slice(0, -2) + "xx", 1795000100_000)).rejects.toMatchObject({
      kind: "bad_signature",
    });
    const other = staticKeys("aaaa", { aaaa: key });
    await expect(verifySessionToken(other, GO_TOKEN, 1795000100_000)).rejects.toMatchObject({ kind: "unknown_key" });
    for (const bad of ["x", "ofs_", "ofs_a.b.c", "ofs_" + "a".repeat(5000)]) {
      await expect(verifySessionToken(keys, bad)).rejects.toMatchObject({ kind: "malformed" });
    }
  });
  it("hashes old and new bootstrap tokens", async () => {
    const newTok = "ofb_default.run_x." + "A".repeat(43);
    expect(await hashBootstrap(newTok)).toMatch(/^[0-9a-f]{64}$/);
    expect(await hashBootstrap("ofb_" + "A".repeat(43))).toMatch(/^[0-9a-f]{64}$/);
    await expect(hashBootstrap("ofb_short")).rejects.toThrow();
    await expect(hashBootstrap("ofs_x")).rejects.toThrow();
  });
});
