import { useQueries } from "@tanstack/react-query";
import { api, unwrap, type Schemas } from "../api/client";

type Secret = Schemas["Secret"];

/** A secret that an agent of a factory can use, with the scope it comes from. */
export type UsableSecret = Secret & { scope: "team" | "factory" };

/** The query of the team secrets (secrets without a factory). */
export const teamSecretsQuery = {
  queryKey: ["secrets", "team"] as const,
  queryFn: () => unwrap(api.GET("/secrets")).then((r) => (r.items ?? []) as Secret[]),
};

/** The query of the secrets that belong to one factory. */
export const factorySecretsQuery = (fid: string) => ({
  queryKey: ["secrets", fid] as const,
  queryFn: () =>
    unwrap(api.GET("/secrets", { params: { query: { factory_id: fid } } })).then((r) => ((r.items ?? []) as Secret[]).filter((s) => s.factory_id === fid)),
});

/**
 * Merges team and factory secrets by name. A factory secret overrides a team
 * secret with the same name, as it does at run time. The result is sorted by name.
 */
export function mergeSecrets(team: Secret[], factory: Secret[]): UsableSecret[] {
  const byName = new Map<string, UsableSecret>();
  for (const s of team) if (!s.factory_id) byName.set(s.name, { ...s, scope: "team" });
  for (const s of factory) byName.set(s.name, { ...s, scope: "factory" });
  return [...byName.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/** The team and factory secrets that the agents of factory `fid` can use. */
export function useUsableSecrets(fid: string): { data: UsableSecret[] | undefined; isLoading: boolean } {
  const [team, factory] = useQueries({ queries: [teamSecretsQuery, factorySecretsQuery(fid)] });
  if (!team.data && !factory.data) return { data: undefined, isLoading: team.isLoading || factory.isLoading };
  return { data: mergeSecrets(team.data ?? [], factory.data ?? []), isLoading: team.isLoading || factory.isLoading };
}

/** Model credential secret types per harness (the same table as the control plane run planner). */
export const HARNESS_CREDENTIAL_TYPES: Record<string, string[]> = {
  claude: ["anthropic_api_key", "anthropic_oauth_token"],
  codex: ["openai_api_key"],
};

/** The default model credential that a run without harness.auth gets. */
export type DefaultCredential = { kind: "one"; name: string; scope: "team" | "factory" } | { kind: "ambiguous"; scope: "team" | "factory"; names: string[] } | { kind: "none" };

/**
 * Applies the server rule (findDefaultCredential of the control plane) to usable
 * secrets from mergeSecrets, which already drops team secrets that a factory
 * secret shadows: the one fitting factory secret, else the one fitting team
 * secret. More than one fit at a scope gives no credential.
 */
export function defaultCredential(secrets: UsableSecret[], types: string[]): DefaultCredential {
  for (const scope of ["factory", "team"] as const) {
    const fits = secrets.filter((s) => s.scope === scope && types.includes(s.type));
    if (fits.length === 1) return { kind: "one", name: fits[0].name, scope };
    if (fits.length > 1) return { kind: "ambiguous", scope, names: fits.map((s) => s.name).sort() };
  }
  return { kind: "none" };
}

/**
 * The credential state of a factory for the overview hint: "one" when some
 * harness has exactly one default credential, else the first ambiguous result,
 * else "none".
 */
export function factoryCredentialState(secrets: UsableSecret[]): DefaultCredential {
  const results = Object.values(HARNESS_CREDENTIAL_TYPES).map((types) => defaultCredential(secrets, types));
  return results.find((r) => r.kind === "one") ?? results.find((r) => r.kind === "ambiguous") ?? { kind: "none" };
}
