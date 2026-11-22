// The capability gate shared by DispatchDO and the provider adapters.
//
// - A restricted network policy on an `egress: none` target is refused with
//   invalid_configuration unless OF_ALLOW_UNENFORCED_NETWORK_POLICY is set
//   (port of egress.Gate, internal/sandbox/egress/probe.go at go-final).
// - Machine size (vCPUs, memory, disk) maps to the nearest provider instance
//   type at or above it. A request above the largest type is invalid_configuration.
// - Arch comes from the provider capabilities, never from the control plane host.

import type { FailureCode } from "@openfactory/core";
import {
  ProviderError,
  TASK_ENV,
  type Arch,
  type Egress,
  type EgressLock,
  type InstanceType,
  type PreflightResult,
  type ProviderCapabilities,
  type SandboxTask,
  type TaskNetworkPolicy,
  type TaskResources,
} from "../types";

/** Control-plane variable that lets a restricted policy run on an egress "none" target. */
export const ENV_ALLOW_UNENFORCED_NETWORK_POLICY = "OF_ALLOW_UNENFORCED_NETWORK_POLICY";
/** Runner variable: comma-separated loopback ports of provider daemons (internal/sandbox/netlock). */
export const ENV_LOOPBACK_DAEMON_PORTS = "OF_LOOPBACK_DAEMON_PORTS";

/** Parses a boolean Worker var: "true", "1", "yes" and "on" are true. */
export function envFlag(v: string | undefined | null): boolean {
  return ["true", "1", "yes", "on"].includes((v ?? "").trim().toLowerCase());
}

export function isRestricted(policy: TaskNetworkPolicy | undefined): policy is TaskNetworkPolicy {
  return policy?.mode === "restricted";
}

// ---------- Hosts (netguard semantics, internal/sandbox/netguard/netguard.go) ----------

/** Lowercases a host and strips a port, IPv6 brackets and a trailing dot. */
export function normalizeHost(h: string): string {
  let s = h.trim().toLowerCase();
  if (s === "") return "";
  if (s.startsWith("[")) {
    const end = s.indexOf("]");
    s = end >= 0 ? s.slice(1, end) : s.slice(1);
  } else {
    const colons = s.split(":").length - 1;
    if (colons === 1) s = s.slice(0, s.indexOf(":"));
  }
  return s.replace(/\.$/, "");
}

const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;

/** True for an IPv4 or IPv6 literal (with or without brackets or port). */
export function isIpLiteral(host: string): boolean {
  const h = normalizeHost(host);
  if (IPV4.test(h)) return true;
  return h.includes(":") && /^[0-9a-f:.]+(%.+)?$/.test(h);
}

/** Normalized always-allow entries; keeps the "." and "*." prefixes. */
export function normalizeAlwaysAllow(entries: readonly string[] | undefined): string[] {
  const out: string[] = [];
  for (const raw of entries ?? []) {
    let h = raw.trim().toLowerCase();
    let prefix = "";
    if (h.startsWith("*.")) [prefix, h] = ["*.", h.slice(2)];
    else if (h.startsWith(".")) [prefix, h] = [".", h.slice(1)];
    const n = normalizeHost(h);
    if (n) out.push(prefix + n);
  }
  return out;
}

/**
 * Reports whether host may be reached under policy (netguard Allowed):
 * always-allow wins, then deny, then an unrestricted policy allows all,
 * else the host must be an exact allowed host.
 */
export function hostAllowed(policy: TaskNetworkPolicy | undefined, host: string): boolean {
  const h = normalizeHost(host);
  if (h === "") return false;
  for (const a of normalizeAlwaysAllow(policy?.alwaysAllow)) {
    if (a.startsWith("*.")) {
      if (h.endsWith(a.slice(1))) return true;
    } else if (a.startsWith(".")) {
      if (h === a.slice(1) || h.endsWith(a)) return true;
    } else if (h === a) {
      return true;
    }
  }
  if ((policy?.deniedHosts ?? []).some((d) => normalizeHost(d) === h)) return false;
  if (!isRestricted(policy)) return true;
  return policy.allowedHosts.some((a) => normalizeHost(a) === h);
}

/** Effective allowed hosts: normalized, deduplicated, denied hosts removed. */
export function effectiveAllowedHosts(policy: TaskNetworkPolicy): string[] {
  const denied = new Set(policy.deniedHosts.map(normalizeHost));
  const out = new Set<string>();
  for (const h of policy.allowedHosts) {
    const n = normalizeHost(h);
    if (n && !denied.has(n)) out.add(n);
  }
  return [...out];
}

// ---------- Egress ----------

/** OF_EGRESS_LOCK for a chosen egress mode. Only an in-sandbox lock of a restricted task locks. */
export function egressLockFor(egress: Egress, policy: TaskNetworkPolicy | undefined): EgressLock {
  return egress === "in-sandbox" && isRestricted(policy) ? "iptables" : "none";
}

export interface GateOptions {
  /** Value of OF_ALLOW_UNENFORCED_NETWORK_POLICY (string var or boolean). */
  allowUnenforced?: boolean | string;
}

export type GateResult =
  | { ok: true; egress: Egress; lock: EgressLock; enforced: boolean; warning?: string }
  | { ok: false; code: FailureCode; message: string };

/**
 * The restricted-policy gate. `egress` is the mode the provider picked for this
 * task (default: the provider capability).
 */
export function gateNetworkPolicy(
  caps: Pick<ProviderCapabilities, "egress">,
  policy: TaskNetworkPolicy | undefined,
  opts: GateOptions = {},
  egress: Egress = caps.egress,
): GateResult {
  if (!isRestricted(policy)) return { ok: true, egress: "none", lock: "none", enforced: true };
  if (egress !== "none") return { ok: true, egress, lock: egressLockFor(egress, policy), enforced: true };
  const allow = typeof opts.allowUnenforced === "string" ? envFlag(opts.allowUnenforced) : !!opts.allowUnenforced;
  if (allow) {
    return {
      ok: true,
      egress: "none",
      lock: "none",
      enforced: false,
      warning: `the target cannot enforce the network policy, and ${ENV_ALLOW_UNENFORCED_NETWORK_POLICY} is set: the task starts`,
    };
  }
  return {
    ok: false,
    code: "invalid_configuration",
    message:
      "the network policy is restricted, but the target cannot enforce it. Choose a target that enforces egress, " +
      `or set ${ENV_ALLOW_UNENFORCED_NETWORK_POLICY}=true only if another control enforces egress`,
  };
}

// ---------- Arch and resources ----------

/** An error message when the provider does not run arch, else undefined. */
export function checkArch(caps: Pick<ProviderCapabilities, "arch">, arch: Arch): string | undefined {
  if (caps.arch.includes(arch)) return undefined;
  return `the provider runs ${caps.arch.join(", ")} only, the task asks for ${arch}`;
}

/** Largest instance type by vCPUs, then memory, then disk. */
export function largestInstanceType(types: readonly InstanceType[]): InstanceType | undefined {
  return [...types].sort(compareInstanceTypes).at(-1);
}

function compareInstanceTypes(a: InstanceType, b: InstanceType): number {
  return a.vcpus - b.vcpus || a.memoryMiB - b.memoryMiB || (a.diskMiB ?? 0) - (b.diskMiB ?? 0);
}

function fits(t: InstanceType, want: TaskResources): boolean {
  return (
    (want.vcpus ?? 0) <= t.vcpus &&
    (want.memoryMiB ?? 0) <= t.memoryMiB &&
    (want.diskMiB === undefined || t.diskMiB === undefined || want.diskMiB <= t.diskMiB)
  );
}

function describe(want: TaskResources): string {
  const parts: string[] = [];
  if (want.vcpus !== undefined) parts.push(`${want.vcpus} vCPU`);
  if (want.memoryMiB !== undefined) parts.push(`${want.memoryMiB} MiB memory`);
  if (want.diskMiB !== undefined) parts.push(`${want.diskMiB} MiB disk`);
  return parts.join(", ") || "default size";
}

/**
 * Maps a machine request to the smallest instance type at or above it. With
 * `customResources`, the request itself is returned (named "custom") when it
 * fits within the largest type. Throws ProviderError("invalid") when it is
 * too large or names an unknown instance type.
 */
export function resolveInstanceType(
  caps: Pick<ProviderCapabilities, "instanceTypes" | "customResources">,
  want: TaskResources,
): InstanceType {
  const types = [...caps.instanceTypes].sort(compareInstanceTypes);
  if (want.instanceType) {
    const named = types.find((t) => t.name === want.instanceType);
    if (!named) throw new ProviderError("invalid", `unknown instance type ${want.instanceType}`);
    if (!fits(named, want))
      throw new ProviderError("invalid", `instance type ${named.name} is smaller than ${describe(want)}`);
    return named;
  }
  for (const n of [want.vcpus, want.memoryMiB, want.diskMiB]) {
    if (n !== undefined && (!Number.isFinite(n) || n < 0)) {
      throw new ProviderError("invalid", `invalid machine size: ${describe(want)}`);
    }
  }
  const max = types.at(-1);
  if (!max) throw new ProviderError("invalid", "the provider declares no instance types");
  if (caps.customResources) {
    if (!fits(max, want)) {
      throw new ProviderError(
        "invalid",
        `the machine asks for ${describe(want)}, more than the provider maximum ${max.name}`,
      );
    }
    if (want.vcpus === undefined && want.memoryMiB === undefined && want.diskMiB === undefined) return types[0]!;
    const min = types[0]!;
    const custom: InstanceType = {
      name: "custom",
      vcpus: want.vcpus ?? min.vcpus,
      memoryMiB: want.memoryMiB ?? min.memoryMiB,
    };
    const disk = want.diskMiB ?? min.diskMiB;
    if (disk !== undefined) custom.diskMiB = disk;
    return custom;
  }
  const hit = types.find((t) => fits(t, want));
  if (!hit) {
    throw new ProviderError(
      "invalid",
      `the machine asks for ${describe(want)}, more than the largest instance type ${max.name}`,
    );
  }
  return hit;
}

/**
 * The shared part of `preflight`: arch, machine size, and (when `gate` is
 * given) the restricted-policy gate. Never throws: errors become
 * `{ok:false, code:"invalid_configuration"}`.
 */
export function preflightTask(
  caps: ProviderCapabilities,
  task: SandboxTask,
  gate?: GateOptions & { egress?: Egress },
): PreflightResult {
  const archErr = checkArch(caps, task.platform.arch);
  if (archErr) return { ok: false, code: "invalid_configuration", message: archErr };
  let type: InstanceType;
  try {
    type = resolveInstanceType(caps, task.resources);
  } catch (e) {
    return { ok: false, code: "invalid_configuration", message: e instanceof ProviderError ? e.detail : String(e) };
  }
  if (gate) {
    const g = gateNetworkPolicy(caps, task.networkPolicy, gate, gate.egress ?? caps.egress);
    if (!g.ok) return g;
  }
  const resources: TaskResources = {
    ...task.resources,
    instanceType: type.name,
    vcpus: type.vcpus,
    memoryMiB: type.memoryMiB,
  };
  if (type.diskMiB !== undefined) resources.diskMiB = type.diskMiB;
  return { ok: true, resources };
}

// ---------- Names, labels and env ----------

/** Task ID as a label value or name part: lowercase, "." → "-", other characters → "-". */
export function sanitizeTaskId(taskId: string): string {
  return taskId
    .toLowerCase()
    .replace(/[^a-z0-9-]/g, "-")
    .replace(/^-+|-+$/g, "");
}

/**
 * Reverses sanitizeTaskId for task IDs of the form `<prefix>_<body>.<session>`
 * (IDs are lower-case base32). Other values are returned unchanged.
 */
export function taskIdFromLabel(label: string): string {
  const m = /^([a-z0-9]+)-([a-z0-9]+)-(\d+)$/.exec(label);
  return m ? `${m[1]}_${m[2]}.${m[3]}` : label;
}

/** Labels every hosted sandbox carries; the orphan sweep keys on them (02 §2.1). */
export function sandboxLabels(installId: string, task: SandboxTask): Record<string, string> {
  return {
    ...(task.labels ?? {}),
    "of-install": installId,
    "of-task": sanitizeTaskId(task.taskId),
    "of-team": sanitizeTaskId(task.teamId),
  };
}

/** The sandbox env: the bootstrap variables plus the lock settings for the chosen egress. */
export function sandboxEnv(
  task: SandboxTask,
  lock: EgressLock,
  loopbackDaemonPorts: readonly number[],
): Record<string, string> {
  const env: Record<string, string> = { ...task.env, [TASK_ENV.egressLock]: lock };
  env[TASK_ENV.runId] ??= task.runId;
  env[TASK_ENV.session] ??= String(task.session);
  if (lock !== "none" && loopbackDaemonPorts.length > 0) env[ENV_LOOPBACK_DAEMON_PORTS] = loopbackDaemonPorts.join(",");
  return env;
}

/** Seconds from a Retry-After header (seconds or HTTP date). */
export function parseRetryAfter(v: string | null | undefined, now: number = Date.now()): number | undefined {
  if (!v) return undefined;
  const n = Number(v.trim());
  if (Number.isFinite(n) && n >= 0) return Math.ceil(n);
  const t = Date.parse(v);
  return Number.isNaN(t) ? undefined : Math.max(0, Math.ceil((t - now) / 1000));
}
