/**
 * Code host coverage: which repositories of a factory an integration record
 * can serve. This mirrors the server rule in internal/forge/forgeutil
 * (RepoOwner, SelectIntegration) and internal/forge (IntegrationProvider,
 * Resolver.Forge). Keep the two in step.
 */

/** The fields of a repository reference that the rule reads. */
export type RepoLike = { codeForge?: string; owner: string; name: string };

/** The fields of an integration record that the rule reads. */
export type IntegrationLike = {
  id?: string;
  provider: string;
  config?: Record<string, string>;
  factory_id?: string;
  status?: string;
};

/** Why no record serves a repository. */
export type GapReason = "missing" | "inactive" | "ambiguous";

export type CoverageGap = { repo: string; reason: GapReason };

/** Integration providers that host code. */
export const FORGE_PROVIDERS = ["github", "gitlab", "azure_devops", "git"];

/** Maps a repository code forge (GITHUB, GITLAB, ...) to its integration provider. */
export function integrationProvider(codeForge?: string): string {
  const f = (codeForge ?? "").trim().replace(/-/g, "_").toUpperCase();
  switch (f) {
    case "":
    case "GITHUB":
      return "github";
    case "GITLAB":
      return "gitlab";
    case "AZURE_DEVOPS":
      return "azure_devops";
    case "GIT":
    case "OTHER":
    case "GENERIC":
      return "git";
  }
  return (codeForge ?? "").toLowerCase();
}

/**
 * Returns the owner of a repository ("owner/name") that config.org of a
 * record must match: the first path segment for github and gitlab, the
 * organization of "org/project/name" for azure_devops, and "" when the
 * repository does not hold one.
 */
export function repoOwner(provider: string, repo: string): string {
  const parts = repo.trim().replace(/^\/+|\/+$/g, "").split("/");
  switch (provider.toLowerCase()) {
    case "git":
      return "";
    case "azure_devops":
      return parts.length >= 3 ? parts[0] : "";
  }
  return parts.length >= 2 ? parts[0] : "";
}

function isActive(i: IntegrationLike): boolean {
  return !i.status || i.status === "active";
}

/**
 * Selects the record for one repository. It takes the records of provider
 * whose config.org is owner (case ignored; an empty owner matches each
 * record), then uses the record of factoryID, else the team record. A record
 * of another factory never counts. Two or more records in the deciding tier
 * are ambiguous: the server refuses to pick one.
 */
export function selectIntegration(
  ints: IntegrationLike[],
  provider: string,
  factoryID: string,
  owner: string,
): { record?: IntegrationLike; reason?: GapReason } {
  const tier = (fac: string) =>
    ints.filter(
      (i) =>
        i.provider.toLowerCase() === provider.toLowerCase() &&
        (i.factory_id ?? "") === fac &&
        (owner === "" || (i.config?.org ?? "").trim().toLowerCase() === owner.toLowerCase()),
    );
  const tiers = factoryID ? [tier(factoryID), tier("")] : [tier("")];
  for (const t of tiers) {
    if (t.length === 0) continue;
    if (t.length > 1) return { reason: "ambiguous" };
    return isActive(t[0]) ? { record: t[0] } : { record: t[0], reason: "inactive" };
  }
  return { reason: "missing" };
}

/** Returns the repository as the user writes it: owner/name. */
export function repoName(r: RepoLike): string {
  return `${r.owner}/${r.name}`;
}

/**
 * Returns the repositories that no active record serves. factoryID is the
 * factory of the repositories; pass "" for a factory that does not exist yet
 * (then only team records count).
 */
export function coverageGaps(repos: RepoLike[], ints: IntegrationLike[], factoryID: string): CoverageGap[] {
  const gaps: CoverageGap[] = [];
  for (const r of repos) {
    const provider = integrationProvider(r.codeForge);
    const name = repoName(r);
    const { reason } = selectIntegration(ints, provider, factoryID, repoOwner(provider, name));
    if (reason) gaps.push({ repo: name, reason });
  }
  return gaps;
}

/** Reports whether one active code host record applies to factoryID (team or factory record). */
export function hasApplicableForge(ints: IntegrationLike[], factoryID: string): boolean {
  return ints.some((i) => FORGE_PROVIDERS.includes(i.provider.toLowerCase()) && isActive(i) && (!i.factory_id || i.factory_id === factoryID));
}

/** Describes the gaps in one short sentence each, grouped by reason. */
export function describeGaps(gaps: CoverageGap[]): string[] {
  const by = (r: GapReason) => gaps.filter((g) => g.reason === r).map((g) => g.repo);
  const out: string[] = [];
  const missing = by("missing");
  const inactive = by("inactive");
  const ambiguous = by("ambiguous");
  if (missing.length) out.push(`No code host covers ${missing.join(", ")}.`);
  if (inactive.length) out.push(`The code host record for ${inactive.join(", ")} is not active.`);
  if (ambiguous.length) out.push(`Several code host records match ${ambiguous.join(", ")}. Keep one.`);
  return out;
}
