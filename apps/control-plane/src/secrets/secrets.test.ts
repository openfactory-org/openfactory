// Ports of internal/secrets and internal/keys tests. The golden envelope was
// produced by the go-final Encrypt code path.
import { describe, expect, it } from "vitest";
import { Envelopes, parseEnvelope } from "./envelope";
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
    for (const bad of ["", "lower", "1ABC", "OF_TOKEN", "A".repeat(129)]) expect(() => validateName(bad), bad).toThrow();
  });
  it("checks values by type", () => {
    validateValue("A", "generic", "x y");
    expect(() => validateValue("A", "anthropic_api_key", "a b")).toThrow(/whitespace/);
    expect(() => validateValue("A", "generic", "")).toThrow(/required/);
    expect(() => validateValue("A", "generic", "a\0b")).toThrow(/NUL/);
    expect(() => validateValue("A", "generic", "x".repeat(128 << 10))).toThrow(/too large/);
    validateValue("A", "docker_registry", JSON.stringify({ registry: "https://Index.Docker.io/", username: "u", password: "p" }));
    expect(() => validateValue("A", "docker_registry", JSON.stringify({ registry: "r", username: "u", password: "p", x: 1 }))).toThrow(/unknown field/);
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
  it("redacts longer values first and keeps short values", () => {
    expect(redact("token abcdef and abc", ["abc", "abcdef"])).toBe("token [REDACTED] and abc");
  });
});
