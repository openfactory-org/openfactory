import { describe, expect, it } from "vitest";
import {
  b64url,
  constantTimeEqual,
  fromAnyB64,
  fromB64url,
  fromHex,
  hmacSha256Hex,
  intakeVerified,
  ownerRule,
  repoOwner,
  selectRecord,
  sha256Hex,
  timingSafeEqual,
  toHex,
  tryFromB64url,
  utf8,
  verifyHmacSha256Hex,
} from "../src/index";

describe("bytes", () => {
  it("encodes hex from bytes and buffers", () => {
    expect(toHex(new Uint8Array([0, 15, 255]))).toBe("000fff");
    expect(toHex(new Uint8Array([1, 2]).buffer)).toBe("0102");
    expect([...fromHex("0aFF")]).toEqual([10, 255]);
    expect(() => fromHex("abc")).toThrow();
    expect(() => fromHex("zz")).toThrow();
  });

  it("decodes base64url without padding strictly", () => {
    const b = utf8("hello?>");
    const s = b64url(b);
    expect(s).not.toMatch(/[=+/]/);
    expect(fromB64url(s)).toEqual(b);
    expect(tryFromB64url(s + "=")).toBeNull();
    expect(tryFromB64url("a")).toBeNull();
    expect(tryFromB64url("a+b")).toBeNull();
    expect(() => fromB64url("a")).toThrow();
    expect(fromAnyB64("aGk=")).toEqual(utf8("hi"));
    expect(fromAnyB64("aGk")).toEqual(utf8("hi"));
  });

  it("hashes and signs like the known vectors", async () => {
    expect(await sha256Hex("abc")).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    // RFC 4231 test case 2.
    expect(await hmacSha256Hex("Jefe", "what do ya want for nothing?")).toBe(
      "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
    );
    expect(await hmacSha256Hex(utf8("Jefe"), utf8("what do ya want for nothing?"))).toBe(
      await hmacSha256Hex("Jefe", "what do ya want for nothing?"),
    );
  });

  it("verifies hex HMAC signatures", async () => {
    const sig = await hmacSha256Hex("s3cret", "body");
    for (const [secret, s, want] of [
      ["s3cret", sig, true],
      ["s3cret", "sha256=" + sig, true],
      ["s3cret", " " + sig.toUpperCase() + " ", true],
      ["other", sig, false],
      ["", sig, false],
      ["s3cret", "zz", false],
      ["s3cret", "", false],
      ["s3cret", sig.slice(0, 62), false],
    ] as const) {
      expect(await verifyHmacSha256Hex(secret, "body", s), `${secret} ${s}`).toBe(want);
    }
    expect(await verifyHmacSha256Hex(utf8("s3cret"), utf8("body"), sig)).toBe(true);
    expect(await verifyHmacSha256Hex(new Uint8Array(), "body", sig)).toBe(false);
  });

  it.each([
    ["abc", "abc", true],
    ["abc", "abd", false],
    ["abc", "abcd", false],
    ["abcd", "abc", false],
    ["abc", "abc\u0000", false],
    ["", "", false],
    ["", "a", false],
  ])("constantTimeEqual(%j, %j) = %s", (a, b, want) => {
    expect(constantTimeEqual(a, b)).toBe(want);
    expect(constantTimeEqual(utf8(a), utf8(b))).toBe(want);
  });

  it("timingSafeEqual compares bytes of any length", () => {
    expect(timingSafeEqual(utf8("ab"), utf8("ab"))).toBe(true);
    expect(timingSafeEqual(utf8("ab"), utf8("abc"))).toBe(false);
    expect(timingSafeEqual("", "")).toBe(true);
  });
});

interface Rec {
  id: string;
  team: string;
  factory?: string;
  org: string;
  sec?: string;
}

const rule = ownerRule<Rec>(
  "github",
  { id: (r) => r.id, team: (r) => r.team, factory: (r) => r.factory ?? "", org: (r) => r.org },
  (msg) => new Error(msg),
);

describe("selectRecord", () => {
  const recs: Rec[] = [
    { id: "team", team: "t", org: " acme " },
    { id: "fac", team: "t", factory: "f1", org: "ACME" },
    { id: "other", team: "t", factory: "f2", org: "globex" },
  ];

  it("prefers the factory record, then the team record", () => {
    expect(selectRecord(recs, rule, "f1", "acme").id).toBe("fac");
    expect(selectRecord(recs, rule, "f3", "Acme").id).toBe("team");
    expect(selectRecord(recs, rule, "", "acme").id).toBe("team");
  });

  it("never falls back to another factory or the first record", () => {
    expect(() => selectRecord(recs, rule, "", "globex")).toThrow(
      'no github integration record for the owner "globex": add a record with config.org "globex"',
    );
    expect(() => selectRecord([recs[1]!], rule, "", "")).toThrow("no github integration record serves this repository");
    const two: Rec[] = [
      { id: "a", team: "t", org: "acme" },
      { id: "b", team: "t", org: "acme" },
    ];
    expect(() => selectRecord(two, rule, "", "acme")).toThrow('2 records (a, b) serve the owner "acme": keep one');
    expect(() => selectRecord(two, rule, "", "")).toThrow(/2 records \(a, b\) can serve this repository/);
  });
});

describe("intakeVerified", () => {
  const errors = {
    ambiguous: (msg: string) => Object.assign(new Error(msg), { kind: "ambiguous" }),
    unverified: (msg: string, noRecord: boolean) => Object.assign(new Error(msg), { kind: "unverified", noRecord }),
  };
  const bySec = (sec: string) => async (r: Rec) => r.sec === sec;
  const recs: Rec[] = [
    { id: "a", team: "ta", org: "acme", sec: "sa" },
    { id: "a_fac", team: "ta", factory: "fa", org: "acme", sec: "sa" },
    { id: "b_fac", team: "tb", factory: "fb", org: "ACME", sec: "sb" },
    { id: "c", team: "tb", org: "other", sec: "sc" },
  ];

  it("picks the record that verifies, whatever the order of the teams", async () => {
    for (const order of [recs, [...recs].reverse()]) {
      expect((await intakeVerified(order, rule, "github", ["acme"], bySec("sa"), errors)).id).toBe("a");
      expect((await intakeVerified(order, rule, "github", ["acme"], bySec("sb"), errors)).id).toBe("b_fac");
    }
  });

  it("reports ambiguous and unverified deliveries", async () => {
    const both: Rec[] = [
      { id: "a", team: "ta", org: "acme", sec: "s" },
      { id: "b", team: "tb", org: "acme", sec: "s" },
    ];
    await expect(intakeVerified(both, rule, "github", ["acme"], bySec("s"), errors)).rejects.toMatchObject({
      kind: "ambiguous",
      message:
        "github: the records a (team ta), b (team tb) verify the delivery: give each team its own webhook secret",
    });
    await expect(intakeVerified(recs, rule, "github", ["acme"], bySec("x"), errors)).rejects.toMatchObject({
      kind: "unverified",
      noRecord: false,
    });
    await expect(intakeVerified(recs, rule, "github", ["nobody"], bySec("sa"), errors)).rejects.toMatchObject({
      kind: "unverified",
      noRecord: true,
      message: 'no github integration record for the owner "nobody"',
    });
  });

  it("names the teams whose records conflict", async () => {
    const conflict: Rec[] = [
      { id: "x1", team: "tx", factory: "f1", org: "acme" },
      { id: "x2", team: "tx", factory: "f2", org: "acme" },
    ];
    const e = (await intakeVerified(conflict, rule, "github", ["acme"], bySec("s"), errors).catch((x) => x)) as Error;
    expect(e.message).toMatch(
      /^github: 0 records can serve the owner "acme" and none verifies the delivery\nteam tx: /,
    );
  });
});

describe("repoOwner", () => {
  it.each([
    ["github", "acme/api", "acme"],
    ["gitlab", "/acme/platform/api/", "acme"],
    ["azure_devops", "proj/repo", ""],
    ["azure_devops", "org/proj/repo", "org"],
    ["git", "a/b", ""],
    ["github", "api", ""],
  ])("%s %s", (provider, repo, want) => {
    expect(repoOwner(provider, repo)).toBe(want);
  });
});
