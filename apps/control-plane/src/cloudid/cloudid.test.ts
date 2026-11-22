// Port of internal/cloudid/cloudid_test.go. Goldens (kid, x, y per seed) come
// from the go-final deriveKey/keyID code path.
import { describe, expect, it } from "vitest";
import { Issuer } from "./issuer";

const enc = new TextEncoder();
const ISS = "https://factory.example.test/";
const GO = {
  "seed-1": {
    kid: "2d12fd30fa615383",
    x: "NGZEixRcAS8XHbDUkxHLBXmFvJb59Ff6ll4nhbB-9Qk",
    y: "tbHfnfVtGcEwZiLZHTHHHH5upt9IaaKY5eSFU20KQ0o",
  },
  "seed-2": {
    kid: "b4386d7008b35d81",
    x: "wDogY1Cuwy_rSaLikoHrjpxIjLxyC4ac0pUjS-mBX5k",
    y: "k2UdDoCDA0O9hu9cRIdD5B5uN4OOKGPfr10tQ79BJWA",
  },
};
const run = {
  id: "run_1",
  team_id: "default",
  factory_id: "fac_1",
  agent_name: "implement",
  work_item_id: "wi_1",
  config: { type: "IMPLEMENT" },
};

describe("cloud identity issuer", () => {
  it("derives the same key as Go and publishes it in the JWKS", async () => {
    for (const [seed, want] of Object.entries(GO)) {
      const i = await Issuer.fromSeeds(ISS, enc.encode(seed));
      expect(i.kid).toBe(want.kid);
      expect(i.jwks().keys).toEqual([{ kty: "EC", crv: "P-256", use: "sig", alg: "ES256", ...want }]);
    }
    const d = (await Issuer.fromSeeds(ISS, enc.encode("seed"))).discovery();
    expect(d["issuer"]).toBe("https://factory.example.test");
    expect(d["jwks_uri"]).toBe("https://factory.example.test/.well-known/jwks.json");
  });

  it("signs run tokens that verify, with Go claims", async () => {
    const i = await Issuer.fromSeeds(ISS, enc.encode("seed"));
    i.setClock(() => 1_800_000_000_000);
    const { token, expiresAt } = await i.token(run, "//iam.googleapis.com/x", 0, {
      creator: "usr_1",
      workerHost: "server",
    });
    expect(expiresAt).toBe(1_800_003_600_000);
    const header = JSON.parse(atob(token.split(".")[0]!.replace(/-/g, "+").replace(/_/g, "/")));
    expect(header).toEqual({ alg: "ES256", kid: i.kid, typ: "JWT" });
    const c = await i.verify(token);
    expect(c).toMatchObject({
      iss: "https://factory.example.test",
      sub: "team/default/factory/fac_1/agent/implement",
      aud: "//iam.googleapis.com/x",
      nbf: 1_800_000_000 - 30,
      run_as: "user:usr_1",
      creator: "usr_1",
      worker_host: "server",
      agent_type: "IMPLEMENT",
    });
    expect((await i.token({ ...run }, "a")).token).toBeTruthy();
    await expect(i.token(run, " ")).rejects.toThrow(/audience/);
    i.setClock(() => 1_800_004_000_000);
    await expect(i.verify(token)).rejects.toThrow(/expired/);
  });

  it("lists previous keys and verifies their tokens", async () => {
    const old = await Issuer.fromSeeds(ISS, enc.encode("seed-1"));
    const tok = (await old.token(run, "aud")).token;
    const rotated = await Issuer.fromSeeds(ISS, enc.encode("seed-2"), [enc.encode("seed-1")]);
    expect(rotated.jwks().keys.map((k) => k["kid"])).toEqual([GO["seed-2"].kid, GO["seed-1"].kid]);
    expect((await rotated.verify(tok)).sub).toContain("agent/implement");
    const fresh = await Issuer.fromSeeds(ISS, enc.encode("seed-2"));
    await expect(fresh.verify(tok)).rejects.toThrow(/bad signature/);
  });
});
