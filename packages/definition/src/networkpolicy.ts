// Outbound network policies: the built-in policy catalog, version
// selectors, host validation and the factory+agent merge. Port of the parts
// of internal/networkpolicy that the definition compiler uses. A copy lives
// here until the package moves to packages/core.

import { goQuote } from "./gofmt.ts";
import type { EffectiveNetworkPolicy, NetworkPolicy } from "./types.ts";

export const BUILTIN_PREFIX = "builtin/";

export interface Policy {
  name: string;
  description: string;
  latest_version: string;
  versions: PolicyVersion[];
}

export interface PolicyVersion {
  version: string;
  is_latest: boolean;
  allowed_hosts: string[];
}

interface BuiltinPolicy {
  name: string;
  description: string;
  versions: { version: string; hosts: string[] }[];
}

const packageRegistries100 = [
  "registry.npmjs.org",
  "registry.yarnpkg.com",
  "repo.yarnpkg.com",
  "nodejs.org",
  "pypi.org",
  "files.pythonhosted.org",
  "pypi.python.org",
  "crates.io",
  "index.crates.io",
  "static.crates.io",
  "static.rust-lang.org",
  "sh.rustup.rs",
  "rubygems.org",
  "index.rubygems.org",
  "proxy.golang.org",
  "sum.golang.org",
  "go.dev",
  "dl.google.com",
  "repo.maven.apache.org",
  "repo1.maven.org",
  "plugins.gradle.org",
  "services.gradle.org",
  "downloads.gradle.org",
  "api.nuget.org",
  "packagist.org",
  "repo.packagist.org",
  "hex.pm",
  "repo.hex.pm",
  "github.com",
  "codeload.github.com",
  "objects.githubusercontent.com",
  "raw.githubusercontent.com",
  "release-assets.githubusercontent.com",
];

const packageRegistries110 = [
  ...packageRegistries100,
  "jsr.io",
  "npm.jsr.io",
  "bun.sh",
  "deno.land",
  "dl.deno.land",
  "astral.sh",
  "conda.anaconda.org",
  "repo.anaconda.com",
  "gitlab.com",
];

const claudeCode100 = ["api.anthropic.com", "console.anthropic.com", "statsig.anthropic.com"];
const claudeCode110 = [...claudeCode100, "claude.ai", "platform.claude.com"];
const codex100 = ["api.openai.com", "auth.openai.com", "chatgpt.com"];

const builtins: BuiltinPolicy[] = [
  {
    name: BUILTIN_PREFIX + "package-registries",
    description: "Package registries and source or release downloads used by development toolchains.",
    versions: [
      { version: "1.0.0", hosts: packageRegistries100 },
      { version: "1.1.0", hosts: packageRegistries110 },
    ],
  },
  {
    name: BUILTIN_PREFIX + "claude-code",
    description: "Inference and authentication services for Claude Code.",
    versions: [
      { version: "1.0.0", hosts: claudeCode100 },
      { version: "1.1.0", hosts: claudeCode110 },
    ],
  },
  {
    name: BUILTIN_PREFIX + "codex",
    description: "Inference and authentication services for Codex.",
    versions: [{ version: "1.0.0", hosts: codex100 }],
  },
];

interface Semver {
  major: number;
  minor: number;
  patch: number;
  pre: string;
}

function semverString(v: Semver): string {
  return `${v.major}.${v.minor}.${v.patch}` + (v.pre !== "" ? "-" + v.pre : "");
}

function less(v: Semver, o: Semver): boolean {
  if (v.major !== o.major) return v.major < o.major;
  if (v.minor !== o.minor) return v.minor < o.minor;
  if (v.patch !== o.patch) return v.patch < o.patch;
  if (v.pre === o.pre) return false;
  if (v.pre === "") return false;
  if (o.pre === "") return true;
  return v.pre < o.pre;
}

function equal(v: Semver, o: Semver): boolean {
  return v.major === o.major && v.minor === o.minor && v.patch === o.patch && v.pre === o.pre;
}

function parseNums(parts: string[]): number[] | null {
  const out: number[] = [];
  for (const p of parts) {
    if (p === "" || (p.length > 1 && p[0] === "0")) return null;
    if (!/^[0-9]+$/.test(p)) return null;
    const n = Number(p);
    if (!Number.isSafeInteger(n)) return null;
    out.push(n);
  }
  return out;
}

function parseSemver(s: string): Semver | null {
  const dash = s.indexOf("-");
  const core = dash >= 0 ? s.slice(0, dash) : s;
  const pre = dash >= 0 ? s.slice(dash + 1) : "";
  const parts = core.split(".");
  if (parts.length !== 3) return null;
  const nums = parseNums(parts);
  if (nums === null) return null;
  if (dash >= 0 && pre === "") return null;
  return { major: nums[0]!, minor: nums[1]!, patch: nums[2]!, pre };
}

function sortedVersions(b: BuiltinPolicy): Semver[] {
  const vs = b.versions.map((bv) => {
    const v = parseSemver(bv.version);
    if (v === null) throw new Error("networkpolicy: invalid built-in version " + bv.version);
    return v;
  });
  return vs.sort((a, c) => (less(a, c) ? -1 : less(c, a) ? 1 : 0));
}

function latestStable(vs: Semver[]): Semver {
  for (let i = vs.length - 1; i >= 0; i--) if (vs[i]!.pre === "") return vs[i]!;
  return vs[vs.length - 1]!;
}

function findHosts(b: BuiltinPolicy, v: string): string[] {
  return b.versions.find((bv) => bv.version === v)?.hosts ?? [];
}

function caretRange(s: string): { lo: Semver; hi: Semver } | null {
  const parts = s.split(".");
  if (parts.length < 1 || parts.length > 3) return null;
  const nums = parseNums(parts);
  if (nums === null) return null;
  while (nums.length < 3) nums.push(0);
  const lo: Semver = { major: nums[0]!, minor: nums[1]!, patch: nums[2]!, pre: "" };
  let hi: Semver;
  if (nums[0]! > 0 || parts.length === 1) hi = { major: nums[0]! + 1, minor: 0, patch: 0, pre: "" };
  else if (nums[1]! > 0 || parts.length === 2) hi = { major: 0, minor: nums[1]! + 1, patch: 0, pre: "" };
  else hi = { major: 0, minor: 0, patch: nums[2]! + 1, pre: "" };
  return { lo, hi };
}

/** catalog returns every built-in policy with its versions sorted ascending. */
export function catalog(): Policy[] {
  return builtins.map((b) => {
    const vs = sortedVersions(b);
    const latest = semverString(latestStable(vs));
    return {
      name: b.name,
      description: b.description,
      latest_version: latest,
      versions: vs.map((v) => ({
        version: semverString(v),
        is_latest: semverString(v) === latest,
        allowed_hosts: [...findHosts(b, semverString(v))].sort(),
      })),
    };
  });
}

export type ResolveResult = { version: string; hosts: string[]; error?: undefined } | { error: string };

/**
 * resolvePolicy resolves "<policy-name>@<version-selector>". Selectors: an
 * exact version, a caret range (^1, ^1.1, ^1.0.0) or latest.
 */
export function resolvePolicy(ref: string): ResolveResult {
  const t = ref.trim();
  const at = t.indexOf("@");
  const name = at >= 0 ? t.slice(0, at) : t;
  const sel = at >= 0 ? t.slice(at + 1) : "";
  if (at < 0 || name === "" || sel === "") {
    return { error: `policy reference ${goQuote(ref)} must have the form <policy-name>@<version-selector>` };
  }
  const bp = builtins.find((b) => b.name === name);
  if (!bp)
    return { error: `unknown built-in policy ${goQuote(name)} (available: ${builtins.map((b) => b.name).join(", ")})` };
  const vs = sortedVersions(bp);
  let chosen: Semver | null = null;
  if (sel === "latest") {
    chosen = latestStable(vs);
  } else if (sel.startsWith("^")) {
    const r = caretRange(sel.slice(1));
    if (r === null) return { error: `policy reference ${goQuote(ref)}: invalid range ^${sel.slice(1)}` };
    for (let i = vs.length - 1; i >= 0; i--) {
      const v = vs[i]!;
      if (v.pre === "" && !less(v, r.lo) && less(v, r.hi)) {
        chosen = v;
        break;
      }
    }
  } else {
    const want = parseSemver(sel);
    if (want === null) {
      return {
        error: `policy reference ${goQuote(ref)}: invalid version selector ${goQuote(sel)} (use an exact version, ^range, or latest)`,
      };
    }
    chosen = vs.find((v) => equal(v, want)) ?? null;
  }
  if (chosen === null) return { error: `policy reference ${goQuote(ref)} matches no published version of ${name}` };
  return { version: semverString(chosen), hosts: [...findHosts(bp, semverString(chosen))] };
}

// ---- IP addresses (Go net.ParseIP and IP.String) ----

function parseIPv4(s: string): number[] | null {
  const parts = s.split(".");
  if (parts.length !== 4) return null;
  const out: number[] = [];
  for (const p of parts) {
    if (!/^[0-9]{1,3}$/.test(p)) return null;
    if (p.length > 1 && p[0] === "0") return null;
    const n = Number(p);
    if (n > 255) return null;
    out.push(n);
  }
  return out;
}

/** parseIP returns the 16 bytes of an IPv4 or IPv6 literal, or null. */
export function parseIP(s: string): number[] | null {
  if (s.includes(".") && !s.includes(":")) {
    const v4 = parseIPv4(s);
    return v4 === null ? null : [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, ...v4];
  }
  return parseIPv6(s);
}

function parseIPv6(s: string): number[] | null {
  if (s === "" || s.includes("%")) return null;
  const ip: number[] = [];
  let ellipsis = -1;
  let rest = s;
  if (rest.startsWith("::")) {
    ellipsis = 0;
    rest = rest.slice(2);
    if (rest === "") return new Array<number>(16).fill(0);
  }
  while (ip.length < 16) {
    const m = /^[0-9a-fA-F]{1,4}/.exec(rest);
    if (!m) return null;
    if (m[0].length < rest.length && rest[m[0].length] === ".") {
      if (ellipsis < 0 && ip.length !== 12) return null;
      if (ip.length + 4 > 16) return null;
      const v4 = parseIPv4(rest);
      if (v4 === null) return null;
      ip.push(...v4);
      rest = "";
      break;
    }
    const n = parseInt(m[0], 16);
    ip.push(n >> 8, n & 0xff);
    rest = rest.slice(m[0].length);
    if (rest === "") break;
    if (rest[0] !== ":" || rest.length === 1) return null;
    rest = rest.slice(1);
    if (rest[0] === ":") {
      if (ellipsis >= 0) return null;
      ellipsis = ip.length;
      rest = rest.slice(1);
      if (rest === "") break;
    }
  }
  if (rest !== "") return null;
  if (ip.length < 16) {
    if (ellipsis < 0) return null;
    const n = 16 - ip.length;
    ip.splice(ellipsis, 0, ...new Array<number>(n).fill(0));
  } else if (ellipsis >= 0) {
    return null;
  }
  return ip;
}

/** ipString formats 16 IP bytes like Go's net.IP.String. */
export function ipString(ip: number[]): string {
  const isV4 = ip.slice(0, 10).every((b) => b === 0) && ip[10] === 0xff && ip[11] === 0xff;
  if (isV4) return ip.slice(12).join(".");
  const groups: number[] = [];
  for (let i = 0; i < 16; i += 2) groups.push((ip[i]! << 8) | ip[i + 1]!);
  let bestStart = -1;
  let bestLen = 0;
  for (let i = 0; i < 8;) {
    if (groups[i] !== 0) {
      i++;
      continue;
    }
    let j = i;
    while (j < 8 && groups[j] === 0) j++;
    if (j - i > bestLen && j - i >= 2) {
      bestStart = i;
      bestLen = j - i;
    }
    i = j;
  }
  const hexs = groups.map((g) => g.toString(16));
  if (bestStart < 0) return hexs.join(":");
  return hexs.slice(0, bestStart).join(":") + "::" + hexs.slice(bestStart + bestLen).join(":");
}

function isCIDR(s: string): boolean {
  const i = s.indexOf("/");
  const addr = s.slice(0, i);
  const mask = s.slice(i + 1);
  const ip = parseIP(addr);
  if (ip === null || !/^[0-9]+$/.test(mask)) return false;
  const bits = addr.includes(":") ? 128 : 32;
  return Number(mask) <= bits;
}

export type HostResult = { host: string; error?: undefined } | { error: string };

/**
 * normalizeHost validates a host entry and returns its lowercase form. A host
 * is a hostname or a literal IP address; URLs, ports, CIDRs and wildcards are
 * errors.
 */
export function normalizeHost(h: string): HostResult {
  let s = h.trim();
  if (s === "") return { error: "host must not be empty" };
  if (s.includes("://")) return { error: `host ${goQuote(h)} must be a hostname or IP address, not a URL` };
  if (s.includes("*")) return { error: `host ${goQuote(h)} must not contain wildcards` };
  if (s.includes("/")) {
    if (isCIDR(s)) return { error: `host ${goQuote(h)} must not be a CIDR range` };
    return { error: `host ${goQuote(h)} must be a hostname or IP address, not a URL or path` };
  }
  const ip = parseIP(s);
  if (ip !== null) return { host: ipString(ip) };
  if (s.startsWith("[")) return { error: `host ${goQuote(h)} must not include brackets or a port` };
  if (s.includes(":")) return { error: `host ${goQuote(h)} must not include a port` };
  if (s.endsWith(".")) s = s.slice(0, -1);
  s = s.toLowerCase();
  if (new TextEncoder().encode(s).length > 253) return { error: `host ${goQuote(h)} is longer than 253 characters` };
  for (const label of s.split(".")) {
    if (
      label === "" ||
      label.length > 63 ||
      !/^[a-z0-9_-]+$/.test(label) ||
      label.startsWith("-") ||
      label.endsWith("-")
    ) {
      return { error: `host ${goQuote(h)} is not a valid hostname` };
    }
  }
  return { host: s };
}

/** validatePolicy returns the errors of every host and extends reference of p. */
export function validatePolicy(p: NetworkPolicy | null | undefined): string[] {
  if (!p) return [];
  const errs: string[] = [];
  for (const ref of p.extends ?? []) {
    const r = resolvePolicy(ref);
    if (r.error !== undefined) errs.push("extends: " + r.error);
  }
  for (const h of p.allowedHosts ?? []) {
    const r = normalizeHost(h);
    if (r.error !== undefined) errs.push("allowedHosts: " + r.error);
  }
  for (const h of p.deniedHosts ?? []) {
    const r = normalizeHost(h);
    if (r.error !== undefined) errs.push("deniedHosts: " + r.error);
  }
  return errs;
}

/**
 * mergePolicies combines the factory and agent policies. Without a policy at
 * either scope, access is unrestricted. Otherwise allowances of both scopes
 * combine and a denial at either scope removes the host. It returns null when
 * a policy is invalid.
 */
export function mergePolicies(
  factory: NetworkPolicy | null | undefined,
  agent: NetworkPolicy | null | undefined,
): EffectiveNetworkPolicy | null {
  if (!factory && !agent) return { restricted: false, allowed_hosts: null, denied_hosts: null };
  if (validatePolicy(factory).length > 0 || validatePolicy(agent).length > 0) return null;
  const allowed = new Set<string>();
  const denied = new Set<string>();
  for (const p of [factory, agent]) {
    if (!p) continue;
    for (const ref of p.extends ?? []) {
      const r = resolvePolicy(ref);
      if (r.error === undefined) for (const h of r.hosts) allowed.add(h);
    }
    for (const h of p.allowedHosts ?? []) {
      const r = normalizeHost(h);
      allowed.add(r.error === undefined ? r.host : "");
    }
    for (const h of p.deniedHosts ?? []) {
      const r = normalizeHost(h);
      denied.add(r.error === undefined ? r.host : "");
    }
  }
  for (const h of denied) allowed.delete(h);
  return { restricted: true, allowed_hosts: [...allowed].sort(), denied_hosts: [...denied].sort() };
}
