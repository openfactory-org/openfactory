import { describe, expect, it } from "vitest";
import type { ProviderCapabilities, TaskNetworkPolicy } from "../types";
import { ProviderError } from "../types";
import { sampleTask } from "../testing/conformance";
import {
  checkArch,
  egressLockFor,
  envFlag,
  gateNetworkPolicy,
  hostAllowed,
  isIpLiteral,
  normalizeHost,
  parseRetryAfter,
  preflightTask,
  resolveInstanceType,
  sandboxEnv,
  sandboxLabels,
  sanitizeTaskId,
} from "./index";

const restricted: TaskNetworkPolicy = {
  mode: "restricted",
  allowedHosts: ["API.example.com", "registry.npmjs.org", "blocked.example.com", "10.0.0.5"],
  deniedHosts: ["blocked.example.com", "github.com"],
  alwaysAllow: ["github.com", ".openfactory.internal", "*.forge.example"],
};

describe("hostAllowed (netguard TestAllowed cases)", () => {
  const always = restricted.alwaysAllow!;
  it.each<[TaskNetworkPolicy | undefined, string, boolean]>([
    [undefined, "anything.example", true],
    [{ mode: "restricted", allowedHosts: [], deniedHosts: ["bad.example"] }, "bad.example", false],
    [restricted, "api.example.com", true],
    [restricted, "api.example.com:443", true],
    [restricted, "API.EXAMPLE.COM.", true],
    [restricted, "sub.api.example.com", false],
    [restricted, "blocked.example.com", false],
    [restricted, "github.com", true],
    [restricted, "control.openfactory.internal", true],
    [restricted, "openfactory.internal", true],
    [restricted, "git.forge.example", true],
    [restricted, "forge.example", false],
    [restricted, "10.0.0.5", true],
    [restricted, "10.0.0.6", false],
    [restricted, "[::1]:80", false],
    [restricted, "", false],
    [{ mode: "restricted", allowedHosts: [], deniedHosts: [], alwaysAllow: always.slice(0, 0) }, "example.com", false],
  ])("%#: %s", (policy, host, want) => {
    expect(hostAllowed(policy, host)).toBe(want);
  });

  it("a restricted policy with no hosts denies all", () => {
    expect(hostAllowed({ mode: "restricted", allowedHosts: [], deniedHosts: [] }, "x.example")).toBe(false);
  });
});

describe("normalizeHost and isIpLiteral", () => {
  it.each([
    ["Example.COM.", "example.com"],
    ["example.com:8080", "example.com"],
    ["[2001:db8::1]:443", "2001:db8::1"],
    ["2001:db8::1", "2001:db8::1"],
    ["  ", ""],
  ])("%s → %s", (a, b) => expect(normalizeHost(a)).toBe(b));

  it.each([
    ["203.0.113.7", true],
    ["203.0.113.7:443", true],
    ["[2606:4700::1111]", true],
    ["::1", true],
    ["example.com", false],
    ["999.1.1.1", false],
    ["deadbeef.example", false],
  ])("%s is IP: %s", (h, want) => expect(isIpLiteral(h)).toBe(want));
});

describe("gateNetworkPolicy", () => {
  const none = { egress: "none" as const };
  it("passes an unrestricted task on any target", () => {
    expect(gateNetworkPolicy(none, undefined)).toMatchObject({ ok: true, lock: "none", enforced: true });
  });
  it("refuses a restricted policy on egress none", () => {
    const r = gateNetworkPolicy(none, restricted);
    expect(r).toMatchObject({ ok: false, code: "invalid_configuration" });
    if (!r.ok) expect(r.message).toContain("OF_ALLOW_UNENFORCED_NETWORK_POLICY");
  });
  it.each([true, "true", "1"])("allows it with OF_ALLOW_UNENFORCED_NETWORK_POLICY=%s", (v) => {
    expect(gateNetworkPolicy(none, restricted, { allowUnenforced: v })).toMatchObject({ ok: true, enforced: false });
  });
  it.each([false, "false", "", "0"])("refuses with OF_ALLOW_UNENFORCED_NETWORK_POLICY=%s", (v) => {
    expect(gateNetworkPolicy(none, restricted, { allowUnenforced: v })).toMatchObject({ ok: false });
  });
  it("chooses the lock from the egress mode", () => {
    expect(gateNetworkPolicy({ egress: "in-sandbox" }, restricted)).toMatchObject({ ok: true, lock: "iptables" });
    expect(gateNetworkPolicy({ egress: "native" }, restricted)).toMatchObject({ ok: true, lock: "none" });
    expect(gateNetworkPolicy({ egress: "native" }, restricted, {}, "none")).toMatchObject({ ok: false });
    expect(egressLockFor("in-sandbox", undefined)).toBe("none");
  });
});

const caps: ProviderCapabilities = {
  egress: "in-sandbox",
  image: "snapshot",
  isolation: "vm",
  arch: ["amd64"],
  maxLifetimeSeconds: null,
  startRate: [],
  instanceTypes: [
    { name: "large", vcpus: 8, memoryMiB: 16384, diskMiB: 113664 },
    { name: "small", vcpus: 2, memoryMiB: 4096, diskMiB: 52224 },
    { name: "default", vcpus: 4, memoryMiB: 8192, diskMiB: 52224 },
  ],
  loopbackDaemonPorts: [22, 8911],
};

describe("resolveInstanceType", () => {
  it.each([
    [{}, "small"],
    [{ vcpus: 2 }, "small"],
    [{ vcpus: 3 }, "default"],
    [{ memoryMiB: 8192 }, "default"],
    [{ vcpus: 1, memoryMiB: 10000 }, "large"],
    [{ instanceType: "default" }, "default"],
  ])("%j → %s", (want, name) => expect(resolveInstanceType(caps, want).name).toBe(name));

  it.each([[{ vcpus: 9 }], [{ memoryMiB: 20000 }], [{ diskMiB: 200000 }], [{ instanceType: "huge" }], [{ vcpus: -1 }]])(
    "%j is invalid",
    (want) => {
      expect(() => resolveInstanceType(caps, want)).toThrow(ProviderError);
      try {
        resolveInstanceType(caps, want);
      } catch (e) {
        expect((e as ProviderError).kind).toBe("invalid");
      }
    },
  );

  it("returns the request itself with customResources", () => {
    const c = {
      instanceTypes: [
        { name: "max", vcpus: 4, memoryMiB: 12288, diskMiB: 20000 },
        { name: "min", vcpus: 1, memoryMiB: 1024, diskMiB: 3072 },
      ],
      customResources: true,
    };
    expect(resolveInstanceType(c, { vcpus: 3, memoryMiB: 6000 })).toEqual({
      name: "custom",
      vcpus: 3,
      memoryMiB: 6000,
      diskMiB: 3072,
    });
    expect(resolveInstanceType(c, {}).name).toBe("min");
    expect(() => resolveInstanceType(c, { vcpus: 5 })).toThrow(/maximum/);
  });
});

describe("preflightTask", () => {
  it("fills the instance type", () => {
    const r = preflightTask(caps, sampleTask({ resources: { vcpus: 3 } }));
    expect(r).toEqual({ ok: true, resources: { vcpus: 4, memoryMiB: 8192, diskMiB: 52224, instanceType: "default" } });
  });
  it("refuses arm64 on an amd64 provider", () => {
    expect(checkArch(caps, "arm64")).toMatch(/amd64 only/);
    expect(preflightTask(caps, sampleTask({ platform: { os: "linux", arch: "arm64" } }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
  });
  it("refuses a too large machine with invalid_configuration", () => {
    expect(preflightTask(caps, sampleTask({ resources: { vcpus: 64 } }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
  });
  it("applies the policy gate when asked", () => {
    const t = sampleTask({ networkPolicy: restricted });
    expect(preflightTask({ ...caps, egress: "none" }, t, {})).toMatchObject({ ok: false });
    expect(preflightTask({ ...caps, egress: "none" }, t)).toMatchObject({ ok: true });
  });
});

describe("labels and env", () => {
  it("sanitizes task IDs", () => {
    expect(sanitizeTaskId("run_ABC123.1")).toBe("run-abc123-1");
  });
  it("labels a task", () => {
    expect(sandboxLabels("inst1", sampleTask({ taskId: "run_X.2", teamId: "default" }))).toMatchObject({
      "of-install": "inst1",
      "of-task": "run-x-2",
      "of-team": "default",
    });
  });
  it("adds the lock variables", () => {
    const env = sandboxEnv(sampleTask(), "iptables", [22, 8911]);
    expect(env.OF_EGRESS_LOCK).toBe("iptables");
    expect(env.OF_LOOPBACK_DAEMON_PORTS).toBe("22,8911");
    expect(sandboxEnv(sampleTask(), "none", [22]).OF_LOOPBACK_DAEMON_PORTS).toBeUndefined();
  });
  it("parses flags and Retry-After", () => {
    expect(envFlag("TRUE")).toBe(true);
    expect(envFlag(undefined)).toBe(false);
    expect(parseRetryAfter("7")).toBe(7);
    expect(parseRetryAfter(new Date(10_000).toUTCString(), 0)).toBe(10);
    expect(parseRetryAfter("soon")).toBeUndefined();
  });
});
