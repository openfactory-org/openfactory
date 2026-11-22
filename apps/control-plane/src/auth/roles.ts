// Roles and the role authorizer. Port of internal/auth/{auth,defaults}.go
// (Action, ActionForRole, RoleAuthorizer) and the role helpers of
// internal/core/types.go and internal/api/auth.go.
//
// Authorization runs inside the owner DO (plan rule 6): the Worker passes the
// Principal with its team role and factory-role map, and the DO calls
// requireRole with the factory of the resource.
import { OfError } from "@openfactory/core";
import type { Principal, Role } from "../contracts/do-messages";
import type { RouteDef, RouteRole } from "../http/handler";

export const ROLES: readonly Role[] = ["user", "editor", "admin"];

/** Trims and lowercases a role name (core.NormalizeRole). */
export function normalizeRole(r: string | undefined | null): string {
  return (r ?? "").trim().toLowerCase();
}

/** user 1, editor 2, admin 3, unknown 0 (core.RoleRank). */
export function roleRank(r: string | undefined | null): number {
  switch (normalizeRole(r)) {
    case "admin":
      return 3;
    case "editor":
      return 2;
    case "user":
      return 1;
  }
  return 0;
}

export function validRole(r: string | undefined | null): r is Role {
  return roleRank(r) > 0;
}

/** A valid role, normalized; "user" for anything else (auth.effectiveRole). */
export function effectiveRole(r: string | undefined | null): Role {
  return validRole(r) ? (normalizeRole(r) as Role) : "user";
}

/** The role of the principal for one factory: the override if valid, else the team role. */
export function roleFor(p: Principal | undefined | null, factoryId: string): Role | "" {
  if (!p) return "";
  if (factoryId !== "") {
    const r = p.factoryRoles?.[factoryId];
    if (r !== undefined && validRole(r)) return normalizeRole(r) as Role;
  }
  return normalizeRole(p.role) as Role;
}

/** Principal.CanOn. */
export function canOn(p: Principal | undefined | null, factoryId: string, min: Role): boolean {
  return !!p && roleRank(roleFor(p, factoryId)) >= roleRank(min);
}

/**
 * Throws like the Go route wrapper: 401 without a principal, 403 with the
 * Go message when the effective role is too low. "public" and "worker"
 * minimum roles pass.
 */
export function requireRole(p: Principal | undefined | null, factoryId: string, min: RouteRole): Principal {
  if (!p) throw new OfError("authentication_required", "missing credentials: sign in or send an Authorization: Bearer header");
  if (min === "public" || min === "worker") return p;
  if (!canOn(p, factoryId, min)) {
    if (factoryId !== "") {
      throw new OfError(
        "not_authorized",
        `this operation requires the ${min} role; your role for this factory is ${roleFor(p, factoryId)}`,
      );
    }
    throw new OfError("not_authorized", `this operation requires the ${min} role; your role is ${roleFor(p, "")}`);
  }
  return p;
}

// ---------- actions (extension point for custom authorizers) ----------

export type ActionCategory = "read" | "operate" | "write" | "admin";
export type Action = ActionCategory | `${ActionCategory}:${string}`;

export function actionCategory(a: Action): string {
  const i = a.indexOf(":");
  return i < 0 ? a : a.slice(0, i);
}

const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS", "TRACE"]);

export function isSafeMethod(m: string): boolean {
  return SAFE_METHODS.has(m.toUpperCase());
}

/** ActionForRole: admin -> admin, editor -> write, user -> read (safe) or operate. */
export function actionForRole(method: string, min: Role): ActionCategory {
  if (min === "admin") return "admin";
  if (min === "editor") return "write";
  return isSafeMethod(method) ? "read" : "operate";
}

const CATEGORY_ROLE: Record<string, Role> = { read: "user", operate: "user", write: "editor", admin: "admin" };

export interface Resource {
  type?: string;
  id?: string;
  teamId?: string;
  factoryId?: string;
}

/** RoleAuthorizer.Allow. Throws OfError; returns on success. */
export function roleAuthorizerAllow(p: Principal | undefined | null, a: Action, res: Resource = {}): void {
  if (!p) throw new OfError("authentication_required", "authentication required");
  if (res.teamId && res.teamId !== p.teamId) throw new OfError("not_authorized", "resource belongs to another team");
  const need = CATEGORY_ROLE[actionCategory(a)];
  if (!need) throw new OfError("not_authorized", `unknown action "${a}"`);
  const have = roleFor(p, res.factoryId ?? "");
  if (roleRank(have) < roleRank(need)) {
    if (res.factoryId) {
      throw new OfError("not_authorized", `${a} requires the ${need} role; your role for this factory is ${have}`);
    }
    throw new OfError("not_authorized", `${a} requires the ${need} role; your role is ${have}`);
  }
}

// ---------- route scope ----------

const ID_FACTORY_PREFIXES = ["/work-items/", "/inbox/", "/webhooks/", "/benchmark-runs/"];

/**
 * True when the Go route wrapper finds the factory of the request from the
 * path ({uid}, {runId}, or {id} of a work item, inbox item, webhook or
 * benchmark run). Those routes are authorized in the owner DO with the
 * factory role; the others with the team role.
 */
export function isFactoryScoped(route: Pick<RouteDef, "path">): boolean {
  const p = route.path;
  if (p.includes("{uid}") || p.includes("{runId}")) return true;
  return p.includes("{id}") && ID_FACTORY_PREFIXES.some((pre) => p.startsWith(pre));
}

/** Actor string recorded on engine operations ("user:<id>", or "api"). */
export function actorId(p: Principal | undefined | null): string {
  if (!p || !p.userId) return "api";
  return "user:" + p.userId;
}
