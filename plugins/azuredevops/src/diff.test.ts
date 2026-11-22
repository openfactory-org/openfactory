import { describe, expect, it } from "vitest";
import { diffOps, splitLines, unifiedDiff } from "./diff";

describe("unifiedDiff", () => {
  it("is empty for equal texts", () => {
    expect(unifiedDiff("a\nb\n", "a\nb\n")).toBe("");
    expect(unifiedDiff("", "")).toBe("");
  });

  it("shows a change with three lines of context", () => {
    const a = Array.from({ length: 10 }, (_, i) => `l${i + 1}`).join("\n") + "\n";
    const b = a.replace("l5\n", "five\n");
    expect(unifiedDiff(a, b)).toBe("@@ -2,7 +2,7 @@\n l2\n l3\n l4\n-l5\n+five\n l6\n l7\n l8\n");
  });

  it("makes two hunks for two distant changes", () => {
    const a = Array.from({ length: 20 }, (_, i) => `l${i + 1}`).join("\n") + "\n";
    const b = a.replace("l2\n", "x\n").replace("l19\n", "y\n");
    const d = unifiedDiff(a, b);
    expect(d.match(/^@@/gm)).toHaveLength(2);
    expect(d).toContain("-l2\n+x\n");
    expect(d).toContain("-l19\n+y\n");
  });

  it("shows an added and a deleted file", () => {
    expect(unifiedDiff("", "x\ny\n")).toBe("@@ -0,0 +1,2 @@\n+x\n+y\n");
    expect(unifiedDiff("x\n", "")).toBe("@@ -1 +0,0 @@\n-x\n");
  });

  it("puts a removed line before the added one", () => {
    const ops = diffOps(["a", "b"], ["a", "c"]);
    expect(ops.map((o) => o.kind + o.text)).toEqual([" a", "-b", "+c"]);
  });

  it("splits lines without a trailing empty line", () => {
    expect(splitLines("a\nb\n")).toEqual(["a", "b"]);
    expect(splitLines("a\n\n")).toEqual(["a", ""]);
    expect(splitLines("")).toEqual([]);
  });
});
