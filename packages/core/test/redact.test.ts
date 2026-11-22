import { describe, expect, it } from "vitest";
import vectors from "../../../schemas/testdata/redact-vectors.json";
import { canonicalJSON, REDACT_MARK, REDACT_MIN_LEN, Redactor, redactJSON, redactTerms } from "../src/index";

describe("redact vectors from the Go code", () => {
  it("has the same constants", () => {
    expect(REDACT_MARK).toBe(vectors.mark);
    expect(REDACT_MIN_LEN).toBe(vectors.min_len);
  });
  it.each(vectors.terms)("terms: $name", ({ values, min_len, terms }) => {
    expect(redactTerms(values, min_len)).toEqual(terms);
  });
  it.each(vectors.string)("string: $name", ({ secrets, min_len, input, output }) => {
    expect(new Redactor(secrets, min_len).string(input)).toBe(output);
  });
  it.each(vectors.json)("json: $name", ({ secrets, input, output }) => {
    expect(new Redactor(secrets).json(input)).toBe(output);
  });
});

describe("Redactor", () => {
  it("ignores short and empty values and counts values", () => {
    const r = new Redactor();
    r.add("", "short", "long-enough-secret", "long-enough-secret");
    expect(r.size).toBe(1);
    expect(r.string("a long-enough-secret b")).toBe("a *** b");
    expect(r.string("")).toBe("");
  });

  it("returns JSON unchanged without terms", () => {
    expect(redactJSON("{not json", [])).toBe("{not json");
  });

  it("redacts nested data and keys", () => {
    const r = new Redactor(["super-secret-1"]);
    expect(r.data({ cmd: "echo super-secret-1", n: 3, nested: { list: ["a super-secret-1 b", 2] } })).toEqual({
      cmd: "echo ***",
      n: 3,
      nested: { list: ["a *** b", 2] },
    });
    expect(r.data({ "super-secret-1": "x" })).toEqual({ "***": "x" });
    const empty = { a: "b" };
    expect(new Redactor().data(empty)).toBe(empty);
  });

  it("matches the Go-escaped form inside data", () => {
    const secret = 'a"b<c>d&e-secret';
    const r = new Redactor([secret]);
    expect(r.data({ k: `x ${secret} y` })).toEqual({ k: "x *** y" });
    expect(canonicalJSON({ k: secret })).toBe('{"k":"a\\"b\\u003cc\\u003ed\\u0026e-secret"}');
  });
});
