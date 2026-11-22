// Teams, users, roles, principals and credentials. Port of internal/core
// (types.go, principal.go). Field names are the Go JSON names.

import type { Timestamp } from "./common";
import type { Factory } from "./definition";
import { OfError } from "./errors";

export interface Team {
  id: string;
  name: string;
  created_at: Timestamp;
}

export type Role = "admin" | "editor" | "user" | (string & {});

export const ROLE_ADMIN = "admin";
export const ROLE_EDITOR = "editor";
/** The lowest role: it can view everything and run work. */
export const ROLE_USER = "user";

/** UserKind values of User.kind. A person has an empty kind. */
export const USER_KIND_PERSON = "";
export const USER_KIND_SERVICE_ACCOUNT = "service_account";
export const USER_KINDS = [USER_KIND_PERSON, USER_KIND_SERVICE_ACCOUNT] as const;
export type UserKind = (typeof USER_KINDS)[number];
/** The ID prefix of a service account. */
export const SERVICE_ACCOUNT_ID_PREFIX = "sa";

export interface User {
  id: string;
  team_id: string;
  email: string;
  name: string;
  role: Role;
  /** Role overrides for single factories, keyed by factory ID. */
  factory_roles?: Record<string, Role>;
  kind?: UserKind;
  created_at: Timestamp;
}

/** Links a user to an account on an integration. */
export interface ExternalIdentity {
  user_id: string;
  provider: string;
  external_id: string;
  handle: string;
}

export interface APIKey {
  id: string;
  team_id: string;
  user_id: string;
  name: string;
  prefix: string;
  created_at: Timestamp;
  last_used_at?: Timestamp;
}

export const PRINCIPAL_TYPES = ["user", "service_account", "factory"] as const;
export type PrincipalType = (typeof PRINCIPAL_TYPES)[number];

/** Who an action is done for. A run records its creator and its run_as. */
export interface Principal {
  type: PrincipalType;
  id: string;
  name?: string;
}

/** Values of Automation.run_as. */
export const RUN_AS_FACTORY = "factory";
export const RUN_AS_USER_PREFIX = "user:";
export const RUN_AS_SERVICE_ACCOUNT_PREFIX = "serviceAccount:";

export type SecretType =
  "generic" | "anthropic_api_key" | "anthropic_oauth_token" | "openai_api_key" | "docker_registry" | (string & {});

export interface Secret {
  id: string;
  team_id: string;
  factory_id?: string;
  name: string;
  type: SecretType;
  created_at: Timestamp;
  updated_at: Timestamp;
}

// ---------- Roles ----------

/** Trims and lowercases the name of a role. */
export function normalizeRole(r: string): Role {
  return r.trim().toLowerCase();
}

/** Orders the built-in roles: user 1, editor 2, admin 3, and 0 for unknown names. */
export function roleRank(r: string): number {
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

/** The role of the user for one factory: its override, else the team-wide role. */
export function roleFor(u: Pick<User, "role" | "factory_roles"> | null | undefined, factoryId = ""): Role {
  if (!u) return "";
  if (factoryId !== "") {
    const r = u.factory_roles?.[factoryId];
    if (r !== undefined && roleRank(r) > 0) return normalizeRole(r);
  }
  return normalizeRole(u.role);
}

// ---------- Principals ----------

/** "<type>:<id>", or "" for a missing principal. */
export function principalString(p: Principal | null | undefined): string {
  if (!p || p.id === "") return "";
  return p.type + ":" + p.id;
}

/** Reports whether p is a person. */
export function isPerson(p: Principal | null | undefined): boolean {
  return !!p && p.type === "user" && p.id !== "";
}

/** Reports whether u is a service account. */
export function isServiceAccount(u: { kind?: string } | null | undefined): boolean {
  return !!u && u.kind === USER_KIND_SERVICE_ACCOUNT;
}

/** The principal of a user record: a user or a service account. */
export function userPrincipal(
  u: (Pick<User, "id" | "name" | "email"> & { kind?: string }) | null | undefined,
): Principal | null {
  if (!u || u.id === "") return null;
  return {
    type: isServiceAccount(u) ? "service_account" : "user",
    id: u.id,
    name: u.name !== "" ? u.name : u.email,
  };
}

/** The principal of a factory. */
export function factoryPrincipal(f: Pick<Factory, "id" | "name" | "alias"> | null | undefined): Principal | null {
  if (!f) return null;
  return { type: "factory", id: f.id, name: f.name !== "" ? f.name : f.alias };
}

/**
 * Splits a runAs value into its principal type and the email (user) or name
 * (service account). An empty value is the factory. Throws invalid_request.
 */
export function parseRunAs(v: string): { type: PrincipalType; value: string } {
  const s = v.trim();
  if (s === "" || s === RUN_AS_FACTORY) return { type: "factory", value: "" };
  if (s.startsWith(RUN_AS_USER_PREFIX)) {
    const email = s.slice(RUN_AS_USER_PREFIX.length).trim();
    if (email === "" || !email.includes("@")) throw new OfError("invalid_request", `runAs "${s}": want user:<email>`);
    return { type: "user", value: email };
  }
  if (s.startsWith(RUN_AS_SERVICE_ACCOUNT_PREFIX)) {
    const name = s.slice(RUN_AS_SERVICE_ACCOUNT_PREFIX.length).trim();
    if (name === "") throw new OfError("invalid_request", `runAs "${s}": want serviceAccount:<name>`);
    return { type: "service_account", value: name };
  }
  throw new OfError("invalid_request", `runAs "${s}": want factory, user:<email> or serviceAccount:<name>`);
}
