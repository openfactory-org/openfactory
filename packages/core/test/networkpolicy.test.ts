import { describe, expect, it } from "vitest";
import {
  allowed,
  catalog,
  isOfError,
  mergeNetworkPolicies,
  mergePolicies,
  normalizeHost,
  resolvePolicy,
  BUILTIN_PREFIX,
  withRequired,
  type EffectiveNetworkPolicy,
} from "../src/index";

describe("resolvePolicy", () => {
  it.each([
    ["builtin/package-registries@1.0.0", "1.0.0", ""],
    ["builtin/package-registries@1.1.0", "1.1.0", ""],
    ["builtin/package-registries@^1", "1.1.0", ""],
    ["builtin/package-registries@^1.0", "1.1.0", ""],
    ["builtin/package-registries@^1.1.0", "1.1.0", ""],
    ["builtin/package-registries@latest", "1.1.0", ""],
    ["builtin/claude-code@^1", "1.1.0", ""],
    ["builtin/codex@latest", "1.0.0", ""],
    ["builtin/codex@1.0.0", "1.0.0", ""],
    ["builtin/codex@^2", "", "matches no published version"],
    ["builtin/codex@9.9.9", "", "matches no published version"],
    ["builtin/codex@1.0", "", "invalid version selector"],
    ["builtin/codex", "", "must have the form"],
    ["builtin/unknown@1.0.0", "", "unknown built-in policy"],
    ["other/package-registries@1.0.0", "", "unknown built-in policy"],
    ["builtin/codex@^x", "", "invalid range"],
  ])("%s", (ref, want, wantErr) => {
    const r = resolvePolicy(ref);
    if (wantErr) {
      expect(r.error).toContain(wantErr);
      return;
    }
    expect(r.error).toBeUndefined();
    if (r.error === undefined) {
      expect(r.version).toBe(want);
      expect(r.hosts.length).toBeGreaterThan(0);
    }
  });
});

describe("catalog", () => {
  it("lists three sorted policies with one latest version", () => {
    const cat = catalog();
    expect(cat).toHaveLength(3);
    for (const p of cat) {
      expect(p.name.startsWith(BUILTIN_PREFIX)).toBe(true);
      expect(p.versions.filter((v) => v.is_latest)).toHaveLength(1);
      for (const v of p.versions) {
        if (v.is_latest) expect(v.version).toBe(p.latest_version);
        expect([...v.allowed_hosts].sort()).toEqual(v.allowed_hosts);
        for (const h of v.allowed_hosts) expect(normalizeHost(h)).toEqual({ host: h });
      }
    }
    const pr = resolvePolicy("builtin/package-registries@1.0.0");
    expect(pr.error === undefined && pr.hosts).toEqual(expect.arrayContaining(["registry.npmjs.org", "pypi.org", "crates.io"]));
  });
});

describe("normalizeHost", () => {
  it.each([
    ["API.Example.com", "api.example.com", ""],
    ["api.example.com.", "api.example.com", ""],
    ["10.0.0.1", "10.0.0.1", ""],
    ["2001:db8::1", "2001:db8::1", ""],
    ["localhost", "localhost", ""],
    ["https://api.example.com", "", "not a URL"],
    ["api.example.com/path", "", "not a URL"],
    ["api.example.com:443", "", "port"],
    ["[2001:db8::1]:443", "", "brackets"],
    ["10.0.0.0/8", "", "CIDR"],
    ["*.example.com", "", "wildcards"],
    ["", "", "empty"],
    ["bad host", "", "not a valid hostname"],
    ["-bad.example.com", "", "not a valid hostname"],
    ["a..b", "", "not a valid hostname"],
    ["a".repeat(64) + ".com", "", "not a valid hostname"],
  ])("%s", (input, want, err) => {
    const r = normalizeHost(input);
    if (err) expect(r.error).toContain(err);
    else expect(r).toEqual({ host: want });
  });
});

describe("merge", () => {
  const factory = {
    extends: ["builtin/codex@1.0.0"],
    allowedHosts: ["api.example.com"],
    deniedHosts: ["blocked.example.com", "chatgpt.com"],
  };
  const agent = { allowedHosts: ["Docs.Example.com", "blocked.example.com"] };

  it("is unrestricted when neither scope declares", () => {
    const got = mergeNetworkPolicies(null, undefined);
    expect(got.restricted).toBe(false);
    expect(allowed(got, "anything.example.org")).toBe(true);
  });

  it("combines allowances and denials win", () => {
    const got = mergeNetworkPolicies(factory, agent);
    expect(got.restricted).toBe(true);
    const cases: Record<string, boolean> = {
      "api.example.com": true,
      "docs.example.com": true,
      "api.openai.com": true,
      "auth.openai.com:443": true,
      "chatgpt.com": false,
      "blocked.example.com": false,
      "other.example.com": false,
      "sub.api.example.com": false,
      "https://api.example": false,
      "API.EXAMPLE.COM": true,
      "docs.example.com:8080": true,
    };
    for (const [h, want] of Object.entries(cases)) expect(allowed(got, h), h).toBe(want);
    expect(got.allowed_hosts).not.toContain("blocked.example.com");
  });

  it("handles empty and deny-only policies", () => {
    let got = mergeNetworkPolicies({}, null);
    expect(got.restricted).toBe(true);
    expect(got.allowed_hosts).toEqual([]);
    got = mergeNetworkPolicies({}, { allowedHosts: ["x.example.com"] });
    expect(allowed(got, "x.example.com")).toBe(true);
    got = mergeNetworkPolicies({ allowedHosts: ["y.example.com"] }, {});
    expect(allowed(got, "y.example.com")).toBe(true);
    got = mergeNetworkPolicies(null, { deniedHosts: ["bad.example.com"] });
    expect(got.restricted).toBe(true);
    expect(allowed(got, "good.example.com")).toBe(false);
  });

  it("names both scopes for invalid input", () => {
    try {
      mergeNetworkPolicies({ allowedHosts: ["*.x.com"] }, { extends: ["builtin/nope@1"] });
      expect.unreachable();
    } catch (e) {
      expect(isOfError(e, "invalid_request")).toBe(true);
      expect(String(e)).toContain("factory networkPolicy");
      expect(String(e)).toContain("agent networkPolicy");
    }
    expect(mergePolicies({ allowedHosts: ["*.x.com"] }, null)).toBeNull();
  });
});

describe("withRequired", () => {
  it("adds required hosts that a denial cannot block", () => {
    const p = mergeNetworkPolicies({ deniedHosts: ["github.com"] }, null);
    expect(allowed(p, "github.com")).toBe(false);
    const p2 = withRequired(p, "github.com:443", "control.example.com");
    expect(allowed(p2, "github.com")).toBe(true);
    expect(allowed(p2, "control.example.com")).toBe(true);
    expect(allowed(p, "github.com")).toBe(false);
    const un: EffectiveNetworkPolicy = { restricted: false, allowed_hosts: null, denied_hosts: null };
    expect(withRequired(un, "x.com")).toBe(un);
  });
});
