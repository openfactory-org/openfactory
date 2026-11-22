// Egress decisions of the Worker intercept. Pure functions, so the
// Egress WorkerEntrypoint and the tests share them.
//
// The container starts with enableInternet:false. The provider registers
// interceptAllOutboundHttp (port 80) and interceptOutboundHttps("*") (port 443)
// before start, so every HTTP and HTTPS request reaches Egress.fetch. Other
// ports are blocked by the platform. Netguard semantics decide each host.

import { hostAllowed, normalizeHost, type TaskNetworkPolicy } from "@openfactory/sandbox";

/** Host of the blocked-host report. Keeps the OF_NETGUARD_REPORT contract. */
export const REPORT_HOST = "of-egress.internal";
export const REPORT_URL = `http://${REPORT_HOST}/blocked`;
/** The CA the platform uses for the HTTPS intercept. bootstrap.sh trusts it. */
export const INTERCEPT_CA = "/etc/cloudflare/certs/cloudflare-containers-ca.crt";

export interface EgressProps {
  taskId: string;
  /** Hostname of OF_CONTROL_URL: reached through the CONTROL_PLANE service binding. */
  controlHost: string;
  /** Restricted policy, or undefined (all hosts except denied ones). */
  policy?: TaskNetworkPolicy;
  /** Extra hosts the bootstrap needs (the runner download host). */
  bootstrapHosts?: string[];
}

export type EgressDecision =
  | { kind: "control"; host: string }
  | { kind: "report"; host: string }
  | { kind: "blocked"; host: string }
  | { kind: "forward"; host: string };

export function decideEgress(props: EgressProps, url: string): EgressDecision {
  let host: string;
  try {
    host = normalizeHost(new URL(url).hostname);
  } catch {
    return { kind: "blocked", host: "" };
  }
  if (host === normalizeHost(props.controlHost)) return { kind: "control", host };
  if (host === REPORT_HOST) return { kind: "report", host };
  if ((props.bootstrapHosts ?? []).some((h) => normalizeHost(h) === host)) return { kind: "forward", host };
  return hostAllowed(props.policy, host) ? { kind: "forward", host } : { kind: "blocked", host };
}

/** Same text as netguard blockedMessage. */
export function blockedMessage(host: string): string {
  return `Open Factory network policy blocked access to ${normalizeHost(host)}\n`;
}

/** Netguard BlockedHost (JSON of GET /blocked). */
export interface BlockedHost {
  host: string;
  count: number;
  first: string;
  last: string;
}

/** Adds one blocked attempt; returns the new list sorted by first occurrence. */
export function recordBlocked(list: readonly BlockedHost[], host: string, now: Date): BlockedHost[] {
  const h = normalizeHost(host);
  const ts = now.toISOString();
  const out = list.map((b) => ({ ...b }));
  const hit = out.find((b) => b.host === h);
  if (hit) {
    hit.count += 1;
    hit.last = ts;
  } else {
    out.push({ host: h, count: 1, first: ts, last: ts });
  }
  return out.sort((a, b) => (a.first === b.first ? a.host.localeCompare(b.host) : a.first.localeCompare(b.first)));
}
