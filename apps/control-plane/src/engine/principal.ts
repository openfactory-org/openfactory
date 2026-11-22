// Principals of runs. Port of internal/engine/principal.go. Each run records
// its creator (who caused it, can be missing) and its run_as principal (who it
// acts as, never missing):
//
//   - A person or a service account (API, dashboard, Factory MCP): that
//     principal for both.
//   - A mention or a follow-up from a linked account: that user for both.
//   - An automation: the linked user of the actor (or none) as creator, and
//     the runAs of the automation (default: the factory) as run_as.
//   - A child run: the principals of its parent.
//   - A scorer, a benchmark or self-improvement: no creator, the factory.
import { OfError, parseRunAs, runSourceOf } from "@openfactory/core";
import type { Principal } from "./types";

/** The fields of a user record that principals need. */
export interface PrincipalUser {
  id: string;
  team_id?: string;
  email?: string;
  name?: string;
  kind?: string;
}

const SERVICE_ACCOUNT = "service_account";

/** platformTrigger: a run that the platform starts itself (scorer, benchmark, self-improvement). */
export function platformTrigger(triggeredBy: string | undefined): boolean {
  const { source } = runSourceOf(triggeredBy ?? "");
  return source === "SCORER" || source === "BENCHMARK" || source === "SELF_IMPROVEMENT";
}

/** The principal of a factory. */
export function factoryPrincipal(factoryId: string): Principal {
  return { type: "factory", id: factoryId };
}

/**
 * The principal of a user or a service account of the team, or undefined when
 * there is no record or the record belongs to another team.
 */
export function userPrincipal(u: PrincipalUser | undefined, teamId: string): Principal | undefined {
  if (!u || !u.id) return undefined;
  if (u.team_id && teamId && u.team_id !== teamId) return undefined;
  return { type: u.kind === SERVICE_ACCOUNT ? "service_account" : "user", id: u.id };
}

/**
 * resolveRunAs: the principal that a runAs value of an automation names. A
 * principal that does not exist is an invalid_request error: the engine never
 * falls back to another principal. A person matches by email (case does not
 * matter), a service account by name.
 */
export function resolveRunAs(users: readonly PrincipalUser[], factoryId: string, runAs: string): Principal {
  const { type, value } = parseRunAs(runAs);
  if (type === "user") {
    const want = value.toLowerCase();
    const u = users.find((x) => x.kind !== SERVICE_ACCOUNT && (x.email ?? "").toLowerCase() === want);
    if (!u) throw new OfError("invalid_request", `runAs ${JSON.stringify(runAs)}: no user with this email in the team`);
    return { type: "user", id: u.id };
  }
  if (type === "service_account") {
    const u = users.find((x) => x.kind === SERVICE_ACCOUNT && x.name === value);
    if (!u)
      throw new OfError(
        "invalid_request",
        `runAs ${JSON.stringify(runAs)}: no service account with this name in the team`,
      );
    return { type: "service_account", id: u.id };
  }
  return factoryPrincipal(factoryId);
}

/** The input of dispatchPrincipals. */
export interface DispatchPrincipalsInput {
  factoryId: string;
  triggeredBy?: string;
  /** The creator record (API caller, linked actor of an event), if any. */
  creator?: PrincipalUser;
  teamId: string;
  /** Set for a run of an automation: its run_as principal (resolveRunAs). */
  automationRunAs?: Principal;
  /** The run belongs to an automation. */
  automation?: boolean;
}

/** dispatchPrincipals: the creator and the run_as principal of a dispatched run. */
export function dispatchPrincipals(i: DispatchPrincipalsInput): { creator?: Principal; runAs: Principal } {
  const factory = factoryPrincipal(i.factoryId);
  if (platformTrigger(i.triggeredBy)) return { runAs: factory };
  const creator = userPrincipal(i.creator, i.teamId);
  if (i.automation || i.automationRunAs) {
    return { ...(creator ? { creator } : {}), runAs: i.automationRunAs ?? factory };
  }
  return creator ? { creator, runAs: creator } : { runAs: factory };
}

/**
 * runPrincipals: the principals that a new run stores. A platform run has no
 * creator and acts as the factory; a run without run_as acts as the factory.
 */
export function runPrincipals(
  factoryId: string,
  triggeredBy: string | undefined,
  creator: Principal | undefined,
  runAs: Principal | undefined,
): { creator?: Principal; runAs: Principal } {
  if (platformTrigger(triggeredBy)) return { runAs: factoryPrincipal(factoryId) };
  return { ...(creator ? { creator } : {}), runAs: runAs ?? factoryPrincipal(factoryId) };
}

/** The run principal of an API caller (a person, a service account, or a person through the Factory MCP). */
export function callerPrincipal(p: { kind: string; userId?: string } | undefined): Principal | undefined {
  if (!p?.userId) return undefined;
  if (p.kind === SERVICE_ACCOUNT) return { type: "service_account", id: p.userId };
  if (p.kind === "user" || p.kind === "factory_mcp") return { type: "user", id: p.userId };
  return undefined;
}

/** IsApprover (workitems.go): by email (case does not matter), or by name for a service account. */
export function isApprover(approvers: readonly string[], u: PrincipalUser | undefined): boolean {
  if (!u) return false;
  const email = (u.email ?? "").trim();
  return approvers.some((a0) => {
    const a = a0.trim();
    if (email && a.toLowerCase() === email.toLowerCase()) return true;
    return u.kind === SERVICE_ACCOUNT && !!u.name && a === u.name;
  });
}
