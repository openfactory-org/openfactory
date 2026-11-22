import { describe, expect, it } from "vitest";
import {
  OfError,
  doName,
  hashToken,
  isRoutedId,
  newBootstrapToken,
  newId,
  newRoutedId,
  newToken,
  parseBootstrapToken,
  parseTaskId,
  partitionOf,
  taskId,
  workDOName,
} from "../src/index";

describe("ids", () => {
  it("makes plain IDs with the Go shape", () => {
    expect(newId("usr")).toMatch(/^usr_[a-z2-7]{20}$/);
    expect(newId("")).toMatch(/^[a-z2-7]{20}$/);
  });

  it("makes routed IDs that carry their partition token", () => {
    const t = newToken("w");
    expect(t).toMatch(/^w[a-z2-7]{15}$/);
    const run = newRoutedId("run", t);
    expect(run).toMatch(/^run_w[a-z2-7]{27}$/);
    expect(isRoutedId(run, "run")).toBe(true);
    expect(partitionOf(run)).toBe(t);
    expect(partitionOf(taskId(run, 3))).toBe(t);
    expect(workDOName("default", run)).toBe(`team:default|work:${t}`);
    expect(partitionOf(newRoutedId("inb", t))).toBe(t);
  });

  it("refuses IDs without a token as not found", () => {
    expect(() => partitionOf("run_abc")).toThrow(OfError);
    try {
      partitionOf(newId("run"));
    } catch (e) {
      expect((e as OfError).code).toBe("resource_not_found");
    }
  });

  it("hashes deterministic tokens", async () => {
    const a = await hashToken("w", "team", "fac", "action", "target");
    expect(a).toBe(await hashToken("w", "team", "fac", "action", "target"));
    expect(a).not.toBe(await hashToken("w", "team", "fac", "action", "other"));
    expect(a).toMatch(/^w[a-z2-7]{15}$/);
  });

  it("names DOs under the team", () => {
    expect(doName.team("default")).toBe("team:default");
    expect(doName.dispatch("t1")).toBe("team:t1|dispatch");
    expect(doName.ingress("t1", "p")).toBe("team:t1|ingress:p");
  });

  it("parses task IDs like core.ParseTaskID", () => {
    expect(parseTaskId("run_x.2")).toEqual({ runId: "run_x", session: 2 });
    expect(parseTaskId("a.b.10")).toEqual({ runId: "a.b", session: 10 });
    for (const bad of ["nodot", ".1", "run_x.0", "run_x.-1", "run_x.x"]) {
      expect(() => parseTaskId(bad)).toThrow(OfError);
    }
  });

  it("round-trips bootstrap tokens", () => {
    const run = newRoutedId("run", newToken("w"));
    const tok = newBootstrapToken("default", run);
    expect(tok.startsWith("ofb_default.")).toBe(true);
    const p = parseBootstrapToken(tok);
    expect(p.teamId).toBe("default");
    expect(p.runId).toBe(run);
    expect(p.secret).toHaveLength(43);
    expect(() => parseBootstrapToken("ofb_a.b")).toThrow(OfError);
    expect(() => parseBootstrapToken("ofs_x")).toThrow(OfError);
  });
});
