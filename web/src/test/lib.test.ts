import { describe, expect, it } from "vitest";
import { diffFiles, diffLines, hunks } from "../lib/diff";
import { duration, humanizeError, money, parseKVLines, samplingLabel, shortID, stateLabel } from "../lib/format";
import { parseRepoLines } from "../pages/Factories";
import { cleanDefPath } from "../pages/Definition";

describe("diff", () => {
  it("diffs lines with an LCS", () => {
    const ops = diffLines("a\nb\nc\n", "a\nx\nc\n");
    expect(ops.map((o) => `${o.kind}:${o.text}`)).toEqual(["same:a", "del:b", "add:x", "same:c"]);
  });
  it("classifies files and skips unchanged ones", () => {
    const d = diffFiles({ "a.yaml": "1", "b.yaml": "x", "same.md": "s" }, { "a.yaml": "2", "c.yaml": "new", "same.md": "s" });
    expect(d.map((f) => `${f.path}:${f.status}`)).toEqual(["a.yaml:modified", "b.yaml:removed", "c.yaml:added"]);
  });
  it("collapses unchanged runs", () => {
    const a = Array.from({ length: 20 }, (_, i) => `l${i}`).join("\n");
    const b = a.replace("l10", "changed");
    const h = hunks(diffLines(a, b), 2);
    expect(h[0]).toEqual({ kind: "gap", count: 8 });
    expect(h.filter((x) => x.kind === "gap")).toHaveLength(2);
  });
});

describe("format", () => {
  it("formats values", () => {
    expect(shortID("run_abcdefghijkl")).toBe("abcdefgh");
    expect(money(1.5)).toBe("$1.50");
    expect(duration(65000)).toBe("1m 5s");
    expect(parseKVLines("A=1\n# comment\nB = two")).toEqual({ A: "1", B: "two" });
    expect(() => parseKVLines("nope")).toThrow(/KEY=value/);
  });
  it("parses repository lines", () => {
    expect(parseRepoLines("acme/api\nGITLAB:group/sub/repo\nhttps://github.com/acme/web.git")).toEqual([
      { codeForge: "GITHUB", owner: "acme", name: "api" },
      { codeForge: "GITLAB", owner: "group/sub", name: "repo" },
      { codeForge: "GITHUB", owner: "acme", name: "web" },
    ]);
    expect(() => parseRepoLines("BITBUCKET:a/b")).toThrow(/forge/);
    expect(() => parseRepoLines("justname")).toThrow(/owner\/name/);
  });
  it("validates definition paths", () => {
    expect(cleanDefPath(" agents/x/agent.md ")).toBe("agents/x/agent.md");
    expect(() => cleanDefPath("../etc")).toThrow();
    expect(() => cleanDefPath("/abs")).toThrow();
  });
});

describe("stateLabel", () => {
  it("writes every state in one case", () => {
    expect(["SUCCEEDED", "ERROR", "CANCELLED", "in_progress", "", undefined].map(stateLabel)).toEqual(["Succeeded", "Error", "Cancelled", "In progress", "", ""]);
  });
});

describe("samplingLabel", () => {
  it("reads the rate as a percentage, not a ratio", () => {
    expect(samplingLabel(25)).toBe("25%");
    expect(samplingLabel(12.345)).toBe("12.35%");
    expect(samplingLabel(undefined)).toBe("10%");
  });
});

describe("humanizeError", () => {
  it("explains a bare failure code and keeps the code as the detail", () => {
    expect(humanizeError("infrastructure_timeout")).toEqual({
      message: "The run waited too long in the queue or passed the maximum session time.",
      detail: "infrastructure_timeout",
    });
  });
});
