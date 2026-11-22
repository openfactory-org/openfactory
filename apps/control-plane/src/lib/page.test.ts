// Port of the pagination helpers of internal/api/httputil.go and config durations.
import { describe, expect, it } from "vitest";
import { parseDuration, loadKey } from "../config";
import { decodeCursor, encodeCursor, pageFrom, paginate } from "./page";

describe("pagination", () => {
  it("encodes cursors as base64url(<unix-nanos>:<id>)", () => {
    const c = encodeCursor(1_700_000_000_123, "fac_1");
    expect(atob(c.replace(/-/g, "+").replace(/_/g, "/"))).toBe("1700000000123000000:fac_1");
    expect(decodeCursor(c)).toEqual({ ms: 1_700_000_000_123, id: "fac_1" });
    expect(() => decodeCursor("!!")).toThrow(/malformed cursor/);
    expect(() => decodeCursor(btoa("x:y"))).toThrow(/malformed cursor/);
  });
  it("reads page parameters", () => {
    expect(pageFrom({})).toEqual({ limit: 50, asc: false });
    expect(pageFrom({ limit: "500", sort_order: "ASC" })).toEqual({ limit: 200, asc: true });
    expect(pageFrom({ limit: "0" }).limit).toBe(50);
    expect(() => pageFrom({ limit: "x" })).toThrow(/integer/);
    expect(() => pageFrom({ sort_by: "name" })).toThrow(/sort_by/);
    expect(() => pageFrom({ sort_order: "up" })).toThrow(/sort_order/);
  });
  it("pages newest first and continues after the cursor", () => {
    const items = [1, 2, 3, 4, 5].map((n) => ({ id: `x${n}`, ms: n }));
    const key = (x: { id: string; ms: number }) => x;
    const p1 = paginate(items, key, { limit: 2, asc: false });
    expect(p1.items.map((x) => x.id)).toEqual(["x5", "x4"]);
    const p2 = paginate(items, key, { limit: 2, asc: false, cursor: decodeCursor(p1.next) });
    expect(p2.items.map((x) => x.id)).toEqual(["x3", "x2"]);
    const p3 = paginate(items, key, { limit: 2, asc: false, cursor: decodeCursor(p2.next) });
    expect(p3).toEqual({ items: [{ id: "x1", ms: 1 }], next: "" });
  });
});

describe("config", () => {
  it("parses Go durations", () => {
    expect(parseDuration("15m")).toBe(900_000);
    expect(parseDuration("1h30m")).toBe(5_400_000);
    expect(parseDuration("1.5s")).toBe(1500);
    expect(parseDuration("0")).toBe(0);
    expect(() => parseDuration("10")).toThrow();
    expect(() => parseDuration("1d")).toThrow();
  });
  it("loads 32-byte keys in every base64 variant", () => {
    const raw = "test-only-fake-secret-key-32byte";
    const std = btoa(raw);
    expect(loadKey(std).length).toBe(32);
    expect(loadKey(std.replace(/=+$/, "")).length).toBe(32);
    expect(loadKey(std.replace(/\+/g, "-").replace(/\//g, "_")).length).toBe(32);
    expect(() => loadKey(btoa("short"))).toThrow(/32 bytes/);
    expect(() => loadKey("@@@")).toThrow(/base64/);
  });
});
