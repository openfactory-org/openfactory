// Golden tests: build() and compute() on a fixed, seeded dataset must give
// the recorded dashboards, value for value and key for key.
import { describe, expect, it } from "vitest";
import { build } from "./build";
import { compute } from "./collect";
import { GOLDEN_SINCE, GOLDEN_UNTIL, fakeIndex, goldenSet } from "./testdata/golden";
import { GOLDEN_BUILD } from "./testdata/golden-build";
import { GOLDEN_COLLECT } from "./testdata/golden-collect";

describe("metrics golden", () => {
  it("build() gives the recorded dashboard", () => {
    const got = build("fac_golden", goldenSet().data, GOLDEN_SINCE, GOLDEN_UNTIL);
    expect(got).toEqual(GOLDEN_BUILD);
    // toEqual ignores key order; the JSON body does not.
    expect(JSON.stringify(got)).toBe(JSON.stringify(GOLDEN_BUILD));
  });

  it("compute() over the read model gives the recorded dashboard", async () => {
    const g = goldenSet();
    const got = await compute(fakeIndex(g), "fac_golden", g.scorers, GOLDEN_SINCE, GOLDEN_UNTIL);
    expect(got).toEqual(GOLDEN_COLLECT);
    expect(JSON.stringify(got)).toBe(JSON.stringify(GOLDEN_COLLECT));
  });

  it("the dataset is deterministic", () => {
    expect(JSON.stringify(goldenSet())).toBe(JSON.stringify(goldenSet()));
  });
});
