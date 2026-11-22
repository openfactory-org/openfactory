import { describe, expect, it } from "vitest";
import { ago, emit, parseKV, rfc3339Local, table, trunc } from "../src/format.ts";

function capture(): { w: { write(c: string): void }; text(): string } {
  let s = "";
  return { w: { write: (c) => void (s += c) }, text: () => s };
}

describe("table", () => {
  it("aligns columns with two spaces of padding and leaves the last cell alone", () => {
    const c = capture();
    table(
      c.w,
      ["ID", "NAME", "TITLE"],
      [
        ["a", "longer name", "x"],
        ["bb", "n", "a title with spaces"],
      ],
    );
    expect(c.text()).toBe(
      ["ID  NAME         TITLE", "a   longer name  x", "bb  n            a title with spaces", ""].join("\n"),
    );
  });

  it("measures characters, not bytes", () => {
    const c = capture();
    table(c.w, ["A", "B"], [["né", "x"]]);
    expect(c.text()).toBe("A   B\nné  x\n");
  });
});

describe("ago", () => {
  const now = new Date("2026-11-22T12:00:00Z");
  it.each([
    ["2026-11-22T11:59:30Z", "30s ago"],
    ["2026-11-22T11:55:00Z", "5m ago"],
    ["2026-11-22T09:00:00Z", "3h ago"],
    [undefined, "-"],
    ["", "-"],
    ["0001-01-01T00:00:00Z", "-"],
  ])("formats %s", (v, want) => {
    expect(ago(v, now)).toBe(want);
  });
  it("shows the local date after two days", () => {
    expect(ago("2026-11-01T12:00:00Z", now)).toMatch(/^2026-1[01]-\d\d$/);
  });
});

describe("trunc", () => {
  it("collapses white space and cuts with an ellipsis", () => {
    expect(trunc("a  b\n c", 10)).toBe("a b c");
    expect(trunc("abcdefghij", 5)).toBe("abcd…");
    expect(trunc(undefined, 5)).toBe("");
  });
});

describe("other helpers", () => {
  it("emits indented JSON", () => {
    const c = capture();
    emit(c.w, { a: [1] });
    expect(c.text()).toBe('{\n  "a": [\n    1\n  ]\n}\n');
  });
  it("parses key=value", () => {
    expect(parseKV(["a=b", "c=d=e"], "--metadata")).toEqual({ a: "b", c: "d=e" });
    expect(parseKV([], "--metadata")).toBeUndefined();
    expect(() => parseKV(["x"], "--metadata")).toThrow('invalid --metadata "x" (want key=value)');
  });
  it("formats RFC 3339", () => {
    expect(rfc3339Local(new Date(2026, 10, 22, 1, 2, 3))).toMatch(/^2026-11-22T01:02:03(Z|[+-]\d\d:\d\d)$/);
  });
});
