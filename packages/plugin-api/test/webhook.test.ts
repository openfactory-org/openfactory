import { describe, expect, it } from "vitest";
import { OfError } from "@openfactory/core";
import type { IntegrationRecord } from "../src/types";
import {
  BodyTooLargeError,
  DeliveryAmbiguousError,
  DeliveryUnverifiedError,
  constantTimeEqual,
  hmacSha256Hex,
  integrationSecret,
  intakeVerified,
  isNoIntegration,
  readBody,
  repoOwner,
  secretScope,
  selectIntegration,
  verifyHmacSha256Hex,
} from "../src/webhook";
import { FakeHostApi } from "../src/testing";

const org = (o: string, extra: Partial<IntegrationRecord> = {}): IntegrationRecord => ({
  id: "x",
  teamId: "t",
  provider: "github",
  name: "n",
  config: { org: o },
  ...extra,
});

describe("signatures", () => {
  it("verifies an HMAC with or without the sha256= prefix", async () => {
    const sig = await hmacSha256Hex("s3cret", "body");
    expect(sig).toMatch(/^[0-9a-f]{64}$/);
    expect(await verifyHmacSha256Hex("s3cret", "body", "sha256=" + sig)).toBe(true);
    expect(await verifyHmacSha256Hex("s3cret", "body", sig.toUpperCase())).toBe(true);
    expect(await verifyHmacSha256Hex("other", "body", sig)).toBe(false);
    expect(await verifyHmacSha256Hex("", "body", sig)).toBe(false);
    expect(await verifyHmacSha256Hex("s3cret", "body", "zz")).toBe(false);
  });

  it("compares secrets and never matches an empty value", () => {
    expect(constantTimeEqual("abc", "abc")).toBe(true);
    expect(constantTimeEqual("abc", "abd")).toBe(false);
    expect(constantTimeEqual("abc", "abcd")).toBe(false);
    expect(constantTimeEqual("", "")).toBe(false);
  });

  it("limits the webhook body", async () => {
    expect(await readBody(new Request("https://x", { method: "POST", body: "hello" }))).toBe("hello");
    await expect(readBody(new Request("https://x", { method: "POST", body: "hello" }), 3)).rejects.toBeInstanceOf(
      BodyTooLargeError,
    );
  });
});

describe("repoOwner", () => {
  it.each([
    ["github", "acme/api", "acme"],
    ["gitlab", "acme/platform/api", "acme"],
    ["azure_devops", "Payments/api", ""],
    ["azure_devops", "org1/Payments/api", "org1"],
    ["git", "acme/api", ""],
    ["github", "api", ""],
  ])("%s %s", (provider, repo, want) => {
    expect(repoOwner(provider, repo)).toBe(want);
  });
});

describe("selectIntegration", () => {
  const ints = [
    org("acme", { id: "team-acme" }),
    org("ACME", { id: "fac1-acme", factoryId: "fac_1" }),
    org("other", { id: "fac3-other", factoryId: "fac_3" }),
  ];

  it("lets the record of a factory win and ignores case", () => {
    expect(selectIntegration(ints, "github", "fac_1", "acme").id).toBe("fac1-acme");
    expect(selectIntegration(ints, "github", "fac_2", "acme").id).toBe("team-acme");
    expect(selectIntegration(ints, "github", "", "Acme").id).toBe("team-acme");
  });

  it("fails with invalid_request and names the owner", () => {
    for (const fac of ["fac_1", "fac_2", ""]) {
      let err: unknown;
      try {
        selectIntegration(ints, "github", fac, "other");
      } catch (e) {
        err = e;
      }
      expect(err).toBeInstanceOf(OfError);
      expect((err as OfError).code).toBe("invalid_request");
      expect((err as OfError).message).toContain('"other"');
      expect(isNoIntegration(err)).toBe(true);
    }
  });

  it("takes the one record when there is no owner and fails with several", () => {
    expect(selectIntegration([org("org1", { id: "a" })], "azure_devops", "fac_1", "").id).toBe("a");
    expect(() =>
      selectIntegration([org("org1", { id: "a" }), org("org2", { id: "b" })], "azure_devops", "", ""),
    ).toThrow(/2 records/);
  });
});

describe("intakeVerified", () => {
  const rec = (id: string, team: string, o: string, sec: string, factoryId?: string): IntegrationRecord => ({
    id,
    teamId: team,
    provider: "gitlab",
    name: id,
    config: { org: o, sec },
    ...(factoryId ? { factoryId } : {}),
  });
  const withSec = (sec: string) => async (r: IntegrationRecord) => r.config["sec"] === sec;

  it("picks the team by the record whose secret verifies, whatever the order", async () => {
    const recs = [
      rec("a", "team_a", "acme", "sa"),
      rec("a_fac", "team_a", "acme", "sa", "fac_a"),
      rec("b_fac", "team_b", "ACME", "sb", "fac_b"),
      rec("c", "team_b", "other", "sc"),
    ];
    for (const order of [recs, [...recs].reverse()]) {
      expect((await intakeVerified(order, "gitlab", ["acme"], withSec("sa"))).id).toBe("a");
      expect((await intakeVerified(order, "gitlab", ["acme"], withSec("sb"))).id).toBe("b_fac");
    }
  });

  it("rejects an unverified delivery", async () => {
    const recs = [rec("a", "team_a", "acme", "sa")];
    await expect(intakeVerified(recs, "gitlab", ["acme"], withSec("nope"))).rejects.toBeInstanceOf(
      DeliveryUnverifiedError,
    );
    await expect(intakeVerified(recs, "gitlab", ["unknown"], withSec("sa"))).rejects.toThrow(
      /no gitlab integration record for the owner/,
    );
    await expect(intakeVerified(recs, "github", ["acme"], withSec("sa"))).rejects.toBeInstanceOf(
      DeliveryUnverifiedError,
    );
  });

  it("rejects a delivery that records of two teams verify", async () => {
    const recs = [rec("a", "team_a", "acme", "same"), rec("b", "team_b", "acme", "same")];
    await expect(intakeVerified(recs, "gitlab", ["acme"], withSec("same"))).rejects.toBeInstanceOf(
      DeliveryAmbiguousError,
    );
  });

  it("uses the only factory record of a team when the team has two without a team record", async () => {
    const recs = [rec("f1", "team_a", "acme", "s", "fac_1")];
    expect((await intakeVerified(recs, "gitlab", ["acme"], withSec("s"))).id).toBe("f1");
  });
});

describe("integration secrets", () => {
  it("reads the secret named by the config of a record through the host scope", async () => {
    const r: IntegrationRecord = {
      id: "i",
      teamId: "team_1",
      provider: "gitlab",
      name: "g",
      config: { webhook_secret_ref: "HOOK" },
      secretRef: "TOKEN",
    };
    const host = new FakeHostApi({ integrations: [r], secrets: { team_1: { HOOK: "h", TOKEN: "t" } } });
    expect(secretScope(r)).toBe("team:team_1");
    expect(await integrationSecret(host, r, "webhook_secret_ref")).toBe("h");
    expect(await integrationSecret(host, r, "")).toBe("t");
    expect(host.secretReads[0]).toEqual({ scope: "team:team_1", name: "HOOK" });
    await expect(integrationSecret(host, r, "missing_ref")).rejects.toMatchObject({ code: "resource_not_found" });
  });

  it("refuses a name that no integration references", async () => {
    const host = new FakeHostApi({ integrations: [], secrets: { team_1: { OTHER: "x" } } });
    await expect(host.getSecret("team:team_1", "OTHER")).rejects.toMatchObject({ code: "not_authorized" });
  });
});
