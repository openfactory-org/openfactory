// The integration selection rule (port of forgeutil/select.go and
// integrations/select.go), generic over the record type: the control plane
// uses Integration, plugins use IntegrationRecord.

/** How the selection rule reads a record and reports a failed selection. */
export interface RecordRule<T> {
  id(r: T): string;
  team(r: T): string;
  /** The factory of a factory record; "" for a team record. */
  factory(r: T): string;
  /** Whether r serves key (an owner or a workspace). Not called for an empty key: then every record serves. */
  serves(r: T, key: string): boolean;
  /** The error when no record (tier is empty) or several records (tier.length > 1) serve key. */
  fail(tier: readonly T[], key: string): Error;
}

/**
 * Picks the record of factoryId, else the team record, among the records of
 * one team that serve key. A record of another factory is never used, and the
 * rule never returns "the first record": several records in the first
 * non-empty tier are an error.
 */
export function selectRecord<T>(records: readonly T[], rule: RecordRule<T>, factoryId: string, key: string): T {
  const tier = (fac: string): T[] =>
    records.filter((r) => rule.factory(r) === fac && (key === "" || rule.serves(r, key)));
  const tiers = factoryId !== "" ? [tier(factoryId), tier("")] : [tier("")];
  for (const t of tiers) {
    if (t.length === 1) return t[0]!;
    if (t.length > 1) throw rule.fail(t, key);
  }
  throw rule.fail([], key);
}

/**
 * The owner of a repository for the selection rule: github and gitlab use the
 * first path segment ("acme/platform/api" gives "acme"); azure_devops the
 * organization of "org/project/name" (else ""); git has no owner.
 */
export function repoOwner(provider: string, repo: string): string {
  const parts = repo
    .trim()
    .replace(/^\/+|\/+$/g, "")
    .split("/");
  switch (provider.toLowerCase()) {
    case "git":
      return "";
    case "azure_devops":
      return parts.length >= 3 ? parts[0]! : "";
  }
  return parts.length < 2 ? "" : parts[0]!;
}

/** The fields of a code-forge record that ownerRule reads. */
export interface OwnerFields<T> {
  id(r: T): string;
  team(r: T): string;
  factory(r: T): string;
  /** config.org of the record. */
  org(r: T): string;
}

/**
 * The rule of code-forge records: a record serves an owner when its config.org
 * is the owner (case ignored). `error` wraps each failure message.
 */
export function ownerRule<T>(provider: string, f: OwnerFields<T>, error: (msg: string) => Error): RecordRule<T> {
  return {
    id: f.id,
    team: f.team,
    factory: f.factory,
    serves: (r, owner) => f.org(r).trim().toLowerCase() === owner.toLowerCase(),
    fail(tier, owner) {
      const ids = tier.map(f.id).join(", ");
      if (tier.length > 1) {
        if (owner === "") {
          return error(
            `${provider}: ${tier.length} records (${ids}) can serve this repository: write the repository with its organization, or keep one record`,
          );
        }
        return error(`${provider}: ${tier.length} records (${ids}) serve the owner "${owner}": keep one`);
      }
      if (owner === "") return error(`no ${provider} integration record serves this repository`);
      return error(
        `no ${provider} integration record for the owner "${owner}": add a record with config.org "${owner}"`,
      );
    },
  };
}

/** The errors of intakeVerified. */
export interface IntakeErrors {
  /** Records of several teams verify the delivery. */
  ambiguous(msg: string): Error;
  /** No record verifies the delivery. noRecord: no team has a record for the owners. */
  unverified(msg: string, noRecord: boolean): Error;
}

/** The candidate of one team: its record for key, else its one factory record; null when it has none for key. */
function teamCandidate<T>(ints: readonly T[], rule: RecordRule<T>, key: string): T | null {
  const mine = ints.filter((r) => key === "" || rule.serves(r, key));
  if (mine.length === 0) return null;
  try {
    return selectRecord(mine, rule, "", key);
  } catch (e) {
    const fac = mine.filter((r) => rule.factory(r) !== "");
    if (fac.length === 1) return fac[0]!;
    throw e;
  }
}

/**
 * Selects the record for a webhook delivery about one of keys from the
 * records of one provider of all teams: each team gives at most one candidate,
 * and the delivery goes to the one candidate that `verify` accepts, whatever
 * the order of the teams.
 */
export async function intakeVerified<T>(
  records: readonly T[],
  rule: RecordRule<T>,
  provider: string,
  keys: readonly string[],
  verify: (r: T) => Promise<boolean>,
  errors: IntakeErrors,
): Promise<T> {
  const byTeam = new Map<string, T[]>();
  for (const r of records) {
    const l = byTeam.get(rule.team(r));
    if (l) l.push(r);
    else byTeam.set(rule.team(r), [r]);
  }
  const cands: T[] = [];
  const skipped: string[] = [];
  const seen = new Set<string>();
  for (const key of keys) {
    for (const [team, ints] of byTeam) {
      let r: T | null;
      try {
        r = teamCandidate(ints, rule, key);
      } catch (e) {
        skipped.push(`team ${team}: ${(e as Error).message}`);
        continue;
      }
      if (r === null || seen.has(rule.id(r))) continue;
      seen.add(rule.id(r));
      cands.push(r);
    }
  }
  const ok: T[] = [];
  for (const r of cands) if (await verify(r)) ok.push(r);
  if (ok.length === 1) return ok[0]!;
  if (ok.length > 1) {
    const ids = ok.map((r) => `${rule.id(r)} (team ${rule.team(r)})`).join(", ");
    throw errors.ambiguous(
      `${provider}: the records ${ids} verify the delivery: give each team its own webhook secret`,
    );
  }
  const names = keys.join(", ");
  if (cands.length === 0 && skipped.length === 0) {
    throw errors.unverified(`no ${provider} integration record for the owner "${names}"`, true);
  }
  let msg = `${provider}: ${cands.length} records can serve the owner "${names}" and none verifies the delivery`;
  // Go joined the skipped teams with errors.Join: one line each.
  if (skipped.length > 0) msg += "\n" + skipped.join("\n");
  throw errors.unverified(msg, false);
}
