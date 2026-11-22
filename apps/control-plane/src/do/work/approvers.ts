// CanApprove (internal/engine/workitems.go): who can resolve an approval of a
// factory. ResolveInbox checks it for an approval before the transaction.
import { doName, forbidden, newId, OfError } from "@openfactory/core";
import type { CallMeta } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { isApprover, type PrincipalUser } from "../../engine/principal";

/** The TeamDO reads of CanApprove. Tests replace them. */
export interface ApproverDeps {
  /** The approvers list of factory.yaml (the compiled runtime), or [] without one. */
  approvers(factoryId: string): Promise<string[]>;
  /** The team member, or undefined when there is no such user. */
  user(id: string): Promise<PrincipalUser | undefined>;
}

/**
 * Reports whether a team member can resolve an approval of a factory. Without
 * an approvers list every caller can. With one, the user must exist and match
 * an entry (IsApprover). An empty userID (a sender without a linked account)
 * cannot approve a factory with an approvers list.
 */
export async function canApprove(deps: ApproverDeps, factoryId: string, userId: string): Promise<boolean> {
  const list = await deps.approvers(factoryId);
  if (!list.length) return true;
  if (!userId) return false;
  return isApprover(list, await deps.user(userId));
}

/** The error of a resolve by someone who is not an approver. */
export function notAnApprover(): OfError {
  return forbidden("only the approvers listed in factory.yaml can resolve this approval");
}

export function teamApproverDeps(env: Env, teamId: string): ApproverDeps {
  const team = env.TEAM.getByName(doName.team(teamId));
  const meta = (): CallMeta => ({
    requestId: newId("req"),
    principal: { kind: "service_account", id: "approvers", teamId, role: "admin", factoryRoles: {} },
  });
  return {
    async approvers(factoryId) {
      // LoadFactoryRuntime: a factory that does not exist is an error.
      const snap = (await team.configSnapshot(factoryId)) as { runtime?: { approvers?: string[] | null } } | null;
      return snap?.runtime?.approvers ?? [];
    },
    async user(id) {
      const out = (await team.api(meta(), "getUser", { id }, null)) as PrincipalUser & {
        $error?: { code: string; detail: string };
      };
      if (out?.$error) {
        if (out.$error.code === "resource_not_found") return undefined;
        throw new OfError(out.$error.code as never, `getUser: ${out.$error.detail}`);
      }
      return out;
    },
  };
}

let depsFor: (env: Env, teamId: string) => ApproverDeps = teamApproverDeps;

/** The approver reads of a WorkDO. */
export function approverDeps(env: Env, teamId: string): ApproverDeps {
  return depsFor(env, teamId);
}

/** Replaces the approver reads (tests). Returns a function that restores them. */
export function setApproverDeps(fn: (env: Env, teamId: string) => ApproverDeps): () => void {
  const prev = depsFor;
  depsFor = fn;
  return () => {
    depsFor = prev;
  };
}
