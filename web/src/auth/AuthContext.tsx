import { createContext, useCallback, useContext, useEffect, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { UNAUTHORIZED_EVENT, fetchMe, logout, type LoginProvider, type Me } from "../api/client";

export type Role = "user" | "editor" | "admin";

const RANK: Record<Role, number> = { user: 1, editor: 2, admin: 3 };

/** Maps a role name to a built-in role. */
export function normalizeRole(r: string | undefined | null): Role | null {
  const n = (r ?? "").trim().toLowerCase();
  return n === "user" || n === "editor" || n === "admin" ? n : null;
}

/**
 * The effective role of the caller for one factory: the role override for
 * that factory if one exists, else the team-wide role. This is the same rule
 * as the API, so the dashboard shows an action only when the API allows it.
 */
export function roleFor(me: Me | null, factoryId?: string): Role | null {
  if (!me) return null;
  if (factoryId) {
    const o = normalizeRole(me.factory_roles?.[factoryId]);
    if (o) return o;
  }
  return normalizeRole(me.role);
}

/** Reports whether role is at least min. */
export function atLeast(role: Role | null, min: Role): boolean {
  return !!role && RANK[role] >= RANK[min];
}

export interface AuthState {
  me: Me | null;
  providers: LoginProvider[];
  loading: boolean;
  error: unknown;
  /** The team-wide role is editor or admin (team resources, new factories). */
  canEdit: boolean;
  /** The team-wide role is admin. */
  isAdmin: boolean;
  /** The effective role for a factory is editor or admin. */
  canEditFactory: (factoryId: string) => boolean;
  /** The effective role for a factory is admin. */
  isFactoryAdmin: (factoryId: string) => boolean;
  refresh: () => Promise<void>;
  signOut: () => Promise<void>;
}

const AuthContext = createContext<AuthState | null>(null);

export const ME_KEY = ["me"] as const;

export function AuthProvider({ children }: { children: ReactNode }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ME_KEY, queryFn: fetchMe, staleTime: 60_000, retry: false });

  useEffect(() => {
    // Any 401 from the API means the session ended: re-check identity.
    const onUnauthorized = () => {
      if (q.data?.me) void qc.invalidateQueries({ queryKey: ME_KEY });
    };
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, [qc, q.data?.me]);

  const refresh = useCallback(async () => {
    await qc.invalidateQueries({ queryKey: ME_KEY });
  }, [qc]);

  const signOut = useCallback(async () => {
    try {
      await logout();
    } finally {
      // Drop every cached team resource, then re-read identity (now signed out).
      qc.removeQueries({ predicate: (query) => query.queryKey[0] !== ME_KEY[0] });
      await qc.resetQueries({ queryKey: ME_KEY });
    }
  }, [qc]);

  const me = q.data?.me ?? null;
  const role = roleFor(me);
  const value: AuthState = {
    me,
    providers: q.data?.providers ?? [],
    loading: q.isLoading,
    error: q.error,
    canEdit: atLeast(role, "editor"),
    isAdmin: atLeast(role, "admin"),
    canEditFactory: (fid) => atLeast(roleFor(me, fid), "editor"),
    isFactoryAdmin: (fid) => atLeast(roleFor(me, fid), "admin"),
    refresh,
    signOut,
  };
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used inside <AuthProvider>");
  return ctx;
}
