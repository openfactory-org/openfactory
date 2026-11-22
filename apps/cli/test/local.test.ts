import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { formatValidationError } from "../src/commands/local.ts";
import { parseRepoSpec } from "../src/repo.ts";
import { runCli } from "./helpers.ts";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const tmp = (): string => mkdtempSync(join(tmpdir(), "of-cli-"));

describe("formatValidationError", () => {
  it("formats file, line, field and message", () => {
    expect(
      formatValidationError("defs", { file: "agents/x/agent.md", line: 3, field: "model", message: "is required" }),
    ).toBe("defs/agents/x/agent.md:3: model: is required");
    expect(formatValidationError("defs", { file: "factory.yaml", message: "model is required", field: "model" })).toBe(
      "defs/factory.yaml: model is required",
    );
  });
});

describe("init", () => {
  it("writes a v1 definition that validates", async () => {
    const dir = join(tmp(), "acme");
    const r = await runCli(["init", dir, "--repo", "acme/app"]);
    expect(r.code).toBe(0);
    expect(readFileSync(join(dir, "factory.yaml"), "utf8")).toContain("schemaVersion: v1\n");
    expect(r.out).toContain("created " + join(dir, "factory.yaml") + "\n");
    expect(r.out).toContain("Next: edit the agents");
    const v = await runCli(["validate", dir]);
    expect(v.code).toBe(0);
    expect(v.err).not.toContain("warning");
    expect(v.out).toMatch(
      /: valid \(acme\) — 5 agent\(s\), 0 automation\(s\), 0 webhook\(s\), 0 scorer\(s\), 0 benchmark\(s\)\n$/,
    );
  });

  it("uses the directory name and refuses to overwrite without --force", async () => {
    const dir = join(tmp(), "payments");
    expect((await runCli(["init", dir])).code).toBe(0);
    expect(readFileSync(join(dir, "factory.yaml"), "utf8")).toContain("name: payments");
    const again = await runCli(["init", dir]);
    expect(again.code).toBe(1);
    expect(again.err).toBe(`error: ${dir} already contains factory.yaml (use --force to overwrite)\n`);
    expect((await runCli(["init", dir, "--force", "--agents", "triage"])).code).toBe(0);
  });

  it("rejects a bad repository and an unknown agent", async () => {
    expect((await runCli(["init", tmp(), "--repo", "nope"])).err).toBe(
      'error: invalid repository "nope" (want forge:owner/name)\n',
    );
    const r = await runCli(["init", tmp(), "--agents", "nope"]);
    expect(r.code).toBe(1);
  });

  it("resolves a relative directory against the working directory", async () => {
    const cwd = tmp();
    const r = await runCli(["init", "sub", "--name", "S"], { cwd });
    expect(r.code).toBe(0);
    expect(existsSync(join(cwd, "sub", "factory.yaml"))).toBe(true);
  });
});

describe("validate", () => {
  it("prints file:line errors on stdout and the count on stderr", async () => {
    const dir = tmp();
    writeFileSync(join(dir, "factory.yaml"), "schemaVersion: v1\nname: x\nbogus: 1\n");
    const r = await runCli(["validate", dir]);
    expect(r.code).toBe(1);
    expect(r.out).toContain(join(dir, "factory.yaml") + ":3:");
    expect(r.err).toBe(`${dir}: ${r.out.trimEnd().split("\n").length} error(s)\n`);
  });

  it("requires factory.yaml in a single directory", async () => {
    const r = await runCli(["validate", tmp()]);
    expect(r.code).toBe(1);
    expect(r.out).toContain("factory.yaml is required at the root of the definition");
  });

  it("validates the current directory by default", async () => {
    const cwd = tmp();
    await runCli(["init", ".", "--name", "cur"], { cwd });
    const r = await runCli(["validate"], { cwd });
    expect(r.code).toBe(0);
    expect(r.out).toContain(".: valid (cur)");
  });

  it("checks several directories and skips the ones without factory.yaml", async () => {
    const base = tmp();
    await runCli(["init", join(base, "a"), "--name", "a"]);
    await runCli(["init", join(base, "b"), "--name", "b"]);
    mkdirSync(join(base, "tool"));
    writeFileSync(join(base, "tool", "main.go"), "package main\n");
    const r = await runCli(["validate", join(base, "a"), join(base, "tool"), join(base, "b")]);
    expect(r.code).toBe(0);
    expect(r.out).toContain("skipped (no factory.yaml)");
    expect(r.out.match(/: valid \(/g)).toHaveLength(2);
  });

  it("fails when one of several directories is invalid", async () => {
    const base = tmp();
    await runCli(["init", join(base, "ok"), "--name", "ok"]);
    mkdirSync(join(base, "bad"));
    writeFileSync(join(base, "bad", "factory.yaml"), "name: x\nbogus: 1\n");
    const r = await runCli(["validate", join(base, "ok"), join(base, "bad")]);
    expect(r.code).toBe(1);
    expect(r.out).toContain(": valid (ok)");
  });

  it.each([
    "00-default-agents",
    "01-single-repo-quickstart",
    "03-multi-harness",
    "04-code-review-only",
    "05-incident-response",
    "06-common-automations",
    "07-self-hosted-worker",
  ])("accepts example %s", async (name) => {
    const r = await runCli(["validate", join(repoRoot, "examples", name)]);
    expect(r.err).toBe("");
    expect(r.code).toBe(0);
  });
});

describe("schema", () => {
  it("lists the documents of the current schema version", async () => {
    const r = await runCli(["schema"]);
    expect(r.out).toMatch(/^Schema version v1 \(https:\/\/\S+\)\n {2}agent\.schema\.json\n/);
    expect(r.out).toContain("  factory.schema.json\n");
  });

  it("prints a document by short or full name", async () => {
    const a = await runCli(["schema", "factory"]);
    const b = await runCli(["schema", "factory.schema.json"]);
    expect(a.code).toBe(0);
    expect(a.out).toBe(b.out);
    expect(JSON.parse(a.out)).toMatchObject({ $id: expect.stringContaining("factory.schema.json") });
  });

  it("rejects an unknown document", async () => {
    const r = await runCli(["schema", "nope"]);
    expect(r.err).toBe('error: unknown schema document "nope" (run `openfactory schema` to list them)\n');
  });
});

describe("parseRepoSpec", () => {
  it.each([
    ["acme/app", { codeForge: "GITHUB", owner: "acme", name: "app" }],
    ["gitlab:acme/web", { codeForge: "GITLAB", owner: "acme", name: "web" }],
    ["azure-devops:org/proj/repo", { codeForge: "AZURE_DEVOPS", owner: "org/proj", name: "repo" }],
    ["https://gitlab.com/acme/web.git", { codeForge: "GITLAB", owner: "acme", name: "web" }],
    ["https://github.com/acme/app", { codeForge: "GITHUB", owner: "acme", name: "app" }],
    ["https://dev.azure.com/org/proj/_git/repo", { codeForge: "AZURE_DEVOPS", owner: "org/proj/_git", name: "repo" }],
  ])("parses %s", (input, want) => {
    expect(parseRepoSpec(input)).toEqual(want);
  });
  it.each(["app", "acme/", "/app", ""])("rejects %j", (input) => {
    expect(() => parseRepoSpec(input)).toThrow("invalid repository");
  });
});
