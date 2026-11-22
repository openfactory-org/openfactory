// Ports of internal/secrets and internal/keys tests. The golden envelope was
// produced by the go-final Encrypt code path.
import { describe, expect, it } from "vitest";
import { Envelopes, localEncrypter, MCP_TOKEN_TYPE, parseEnvelope } from "./envelope";
import { expandWith, placeholders, redact } from "./expand";
import { LocalKeys } from "./keys";
import { imageHost, validateName, validateValue } from "./validate";

const enc = new TextEncoder();
const dec = new TextDecoder();
const K1 = enc.encode("test-only-fake-secret-key-32byte");
const K2 = enc.encode("test-only-other-secret-key-32byt");
const id = { teamId: "default", factoryId: "fac_1", name: "API_TOKEN", type: "generic" };
const GO_ENVELOPE =
  "Agg0ZGYwZWIwMgA8A7saKSeRhhSFP09gRIRhSwVXxelrPPSAVL8QweyX15Ul+3GPu8EwRsD2gykOe4K/2Vx+aKutTdwOpXTjmXUbj7p82uQk6E72/nYxAz5dlScn7kJ1mwZy/PgPKQdg7Tk=";

const b64 = (s: string) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

describe("envelope", () => {
  it("the MCP encrypter writes the secret store's envelopes", async () => {
    const env = { OF_SECRET_KEY: btoa(dec.decode(K1)) };
    const mcp = await localEncrypter(env);
    // An envelope written by Go opens through the positional form.
    expect(dec.decode(await mcp.decrypt("default", "fac_1", "API_TOKEN", "generic", b64(GO_ENVELOPE)))).toBe("hunter2");
    const sealed = await mcp.encrypt("team1", "", "mcp-oauth:mcp_1", MCP_TOKEN_TYPE, enc.encode("tok"));
    const store = new Envelopes(await LocalKeys.create(K1));
    const ident = { teamId: "team1", factoryId: "", name: "mcp-oauth:mcp_1", type: MCP_TOKEN_TYPE };
    expect(dec.decode(await store.decrypt(ident, sealed))).toBe("tok");
    // The stricter parser of the store applies: an unknown format version is named.
    const bad = sealed.slice();
    bad[0] = 3;
    await expect(mcp.decrypt("team1", "", "mcp-oauth:mcp_1", MCP_TOKEN_TYPE, bad)).rejects.toThrow(
      "unsupported envelope version 3",
    );
  });

  it("decrypts an envelope written by Go", async () => {
    const e = new Envelopes(await LocalKeys.create(K1));
    expect(e.currentVersion()).toBe("4df0eb02");
    expect(parseEnvelope(b64(GO_ENVELOPE)).version).toBe("4df0eb02");
    expect(dec.decode(await e.decrypt(id, b64(GO_ENVELOPE)))).toBe("hunter2");
    // The identity is bound as additional data.
    await expect(e.decrypt({ ...id, factoryId: "" }, b64(GO_ENVELOPE))).rejects.toThrow(/tampered/);
  });

  it("round-trips, refuses tampering and other keys, and rewraps after rotation", async () => {
    const old = new Envelopes(await LocalKeys.create(K1));
    const ct = await old.encrypt(id, enc.encode("v"));
    const t = ct.slice();
    t[t.length - 1]! ^= 1;
    await expect(old.decrypt(id, t)).rejects.toThrow(/tampered/);
    await expect(new Envelopes(await LocalKeys.create(K2)).decrypt(id, ct)).rejects.toThrow(/different master key/);
    const rotated = new Envelopes(await LocalKeys.create(K2, [K1]));
    expect(dec.decode(await rotated.decrypt(id, ct))).toBe("v");
    const r = await rotated.rewrap(id, ct);
    expect(r.changed).toBe(true);
    expect(parseEnvelope(r.out).version).toBe(rotated.currentVersion());
    expect(dec.decode(await new Envelopes(await LocalKeys.create(K2)).decrypt(id, r.out))).toBe("v");
    expect((await rotated.rewrap(id, r.out)).changed).toBe(false);
    expect(() => parseEnvelope(new Uint8Array([9, 1]))).toThrow(/unsupported/);
  });
});

describe("validate", () => {
  it("checks names", () => {
    validateName("METABASE_API_KEY");
    for (const bad of ["", "lower", "1ABC", "OF_TOKEN", "A".repeat(129)])
      expect(() => validateName(bad), bad).toThrow();
  });
  it("checks values by type", () => {
    validateValue("A", "generic", "x y");
    expect(() => validateValue("A", "anthropic_api_key", "a b")).toThrow(/whitespace/);
    expect(() => validateValue("A", "generic", "")).toThrow(/required/);
    expect(() => validateValue("A", "generic", "a\0b")).toThrow(/NUL/);
    expect(() => validateValue("A", "generic", "x".repeat(128 << 10))).toThrow(/too large/);
    validateValue(
      "A",
      "docker_registry",
      JSON.stringify({ registry: "https://Index.Docker.io/", username: "u", password: "p" }),
    );
    expect(() =>
      validateValue("A", "docker_registry", JSON.stringify({ registry: "r", username: "u", password: "p", x: 1 })),
    ).toThrow(/unknown field/);
    expect(() => validateValue("A", "aws_ecr_credential", "{}")).toThrow(/unknown secret type/);
  });
  it("finds image hosts", () => {
    expect(imageHost("node:22")).toBe("docker.io");
    expect(imageHost("registry.example.com/team/tools:1")).toBe("registry.example.com");
    expect(imageHost("localhost/x")).toBe("localhost");
    expect(imageHost("index.docker.io/library/node")).toBe("docker.io");
  });
});

describe("expand and redact", () => {
  it("expands placeholders and reports missing names together", async () => {
    expect(placeholders("{{A}} {{ B }} {{A}}")).toEqual(["A", "B"]);
    expect(await expandWith("x={{ A }}", (n) => (n === "A" ? "1" : undefined))).toBe("x=1");
    await expect(expandWith("{{X}}{{Y}}", () => undefined)).rejects.toThrow("unknown secret(s) referenced: X, Y");
  });
  // The one grammar of the control plane (go-final internal/secrets/expand.go).
  // The MCP proxy used [A-Z_] names and the plugin host also allowed . / -;
  // both now expand with expandWith.
  const values: Record<string, string> = { A: "1", api_token: "t", B_2: "b", _X: "x" };
  const grammar: Array<{ name: string; in: string; out: string }> = [
    { name: "upper-case name", in: "x={{A}}", out: "x=1" },
    { name: "lower-case name", in: "{{api_token}}", out: "t" },
    { name: "digits and underscore", in: "{{B_2}}{{_X}}", out: "bx" },
    { name: "ASCII whitespace inside", in: "{{ A\t}} {{\n\r\fA }}", out: "1 1" },
    { name: "Unicode space is not whitespace", in: "{{\u00a0A}}", out: "{{\u00a0A}}" },
    { name: "vertical tab is not whitespace", in: "{{\vA}}", out: "{{\vA}}" },
    { name: "leading digit is not a name", in: "{{1A}}", out: "{{1A}}" },
    { name: "dot, slash and dash are not names", in: "{{a.b}} {{x/y-z}}", out: "{{a.b}} {{x/y-z}}" },
    { name: "empty braces", in: "{{}} {{ }}", out: "{{}} {{ }}" },
    { name: "single braces", in: "{A} {{A} {A}}", out: "{A} {{A} {A}}" },
    { name: "no escape syntax", in: "\\{{A}}", out: "\\1" },
    { name: "triple braces keep the outer pair", in: "{{{A}}}", out: "{1}" },
    { name: "nested placeholder", in: "{{ {{A}} }}", out: "{{ 1 }}" },
    { name: "repeated name", in: "{{A}}{{ A }}", out: "11" },
  ];
  it.each(grammar)("grammar: $name", async (c) => {
    expect(await expandWith(c.in, (n) => values[n])).toBe(c.out);
  });

  it("looks up each distinct name once and stops on a lookup error", async () => {
    const calls: string[] = [];
    const lookup = (n: string) => {
      calls.push(n);
      return values[n];
    };
    expect(await expandWith("{{A}} {{api_token}} {{A}} {{ A }}", lookup)).toBe("1 t 1 1");
    expect(calls).toEqual(["A", "api_token"]);
    expect(placeholders("{{a.b}} {{\u00a0A}} {{ok}}")).toEqual(["ok"]);
    await expect(
      expandWith("{{A}}", () => {
        throw new Error("backend down");
      }),
    ).rejects.toThrow("backend down");
  });

  it("reports unknown names together as resource_not_found", async () => {
    await expect(expandWith("{{A}} {{nope}} {{NOPE_2}}", (n) => values[n])).rejects.toMatchObject({
      code: "resource_not_found",
      detail: "unknown secret(s) referenced: nope, NOPE_2",
    });
  });

  it("redacts longer values first and keeps short values", () => {
    expect(redact("token abcdef and abc", ["abc", "abcdef"])).toBe("token [REDACTED] and abc");
  });
});
