// Offline check of the fixture trees (runs in the plain workspace test run).
import { readdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { fixture, renameFactory } from "./live.ts";

const FIXTURES = resolve(dirname(fileURLToPath(import.meta.url)), "../fixtures");
const KNOWN = new Set(["ALLOWED_HOST", "GITHUB_NAME", "GITHUB_OWNER", "GIT_CLONE_URL", "SANDBOX_IMAGE"]);

describe("e2e fixtures", () => {
  const names = readdirSync(FIXTURES, { withFileTypes: true })
    .filter((e) => e.isDirectory())
    .map((e) => e.name);

  it("has the five fixture trees", () => {
    expect(names.sort()).toEqual(["check", "def", "gh", "gh-definition", "net"]);
  });

  for (const name of names) {
    it(`${name}: factory.yaml, a foreman and only known placeholders`, () => {
      const files = fixture(name);
      expect(files["factory.yaml"]).toMatch(/^schemaVersion: v1$/m);
      expect(files["agents/foreman/agent.md"]).toMatch(/agentType: FOREMAN/);
      const placeholders = new Set(
        Object.values(files).flatMap((t) => [...t.matchAll(/__([A-Z_]+)__/g)].map((m) => m[1])),
      );
      for (const p of placeholders) expect(KNOWN.has(p!), `unknown placeholder __${p}__`).toBe(true);
    });
  }

  it("replaces placeholders and renames the factory", () => {
    const files = renameFactory(fixture("gh", { GITHUB_OWNER: "octo", GITHUB_NAME: "demo" }), "renamed");
    expect(files["factory.yaml"]).toContain("owner: octo");
    expect(files["factory.yaml"]).toMatch(/^name: renamed$/m);
    expect(files["automations/issue-labeled/automation.md"]).toContain("octo/demo");
  });
});
