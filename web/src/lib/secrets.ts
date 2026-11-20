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
