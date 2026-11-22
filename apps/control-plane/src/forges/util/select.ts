// The integration selection rule. Port of internal/forge/forgeutil/select.go.
import { OfError, type Integration } from "@openfactory/core";

/** reason of the OfError (invalid_request) when no record serves the owner (Go ErrNoIntegration). */
export const REASON_NO_INTEGRATION = "no_integration";

export function noIntegration(msg: string): OfError {
  return new OfError("invalid_request", `${msg}: no code-forge integration configured`, { reason: REASON_NO_INTEGRATION });
}

export function isNoIntegration(e: unknown): boolean {
  return e instanceof OfError && e.reason === REASON_NO_INTEGRATION;
}

/** No record verifies the signature of a webhook delivery (answer 401). */
export class DeliveryUnverifiedError extends Error {
  override readonly name = "DeliveryUnverifiedError";
  constructor(
    msg: string,
    readonly noIntegration = false,
  ) {
    super(`${msg}: no integration record verifies the delivery`);
  }
}

/** Records of several teams verify one delivery (answer 409). */
export class DeliveryAmbiguousError extends Error {
  override readonly name = "DeliveryAmbiguousError";
  constructor(msg: string) {
    super(`${msg}: integration records of several teams verify the delivery`);
  }
}

const cfg = (in_: Integration, k: string): string => (in_.config?.[k] ?? "").trim();
const eqFold = (a: string, b: string): boolean => a.toLowerCase() === b.toLowerCase();

/**
 * The owner of a repository for the selection rule: github and gitlab use the
 * first path segment; azure_devops the organization of "org/project/name"
 * (else ""); git "".
 */
export function repoOwner(provider: string, repo: string): string {
  const r = repo.trim().replace(/^\/+|\/+$/g, "");
  const parts = r.split("/");
  switch (provider.toLowerCase()) {
    case "git":
      return "";
    case "azure_devops":
      return parts.length >= 3 ? parts[0]! : "";
  }
  if (parts.length < 2) return "";
  return parts[0]!;
}

/**
 * Selects one record of ints (one provider of one team) for factoryId and
 * owner: the records whose config.org is owner (case ignored), the factory
 * record first, else the team record. Never "the first record".
 */
export function selectIntegration(ints: readonly Integration[], provider: string, factoryId: string, owner: string): Integration {
  const tier = (fac: string): Integration[] =>
    ints.filter((in_) => (in_.factory_id ?? "") === fac && (owner === "" || eqFold(cfg(in_, "org"), owner)));
  const tiers = factoryId !== "" ? [tier(factoryId), tier("")] : [tier("")];
  for (const t of tiers) {
    if (t.length === 0) continue;
    if (t.length === 1) return t[0]!;
    const ids = t.map((x) => x.id).join(", ");
    if (owner === "") {
      throw noIntegration(
        `${provider}: ${t.length} records (${ids}) can serve this repository: write the repository with its organization, or keep one record`,
      );
    }
    throw noIntegration(`${provider}: ${t.length} records (${ids}) serve the owner "${owner}": keep one`);
  }
  if (owner === "") throw noIntegration(`no ${provider} integration record serves this repository`);
  throw noIntegration(`no ${provider} integration record for the owner "${owner}": add a record with config.org "${owner}"`);
}

/** Records of one provider across the given records, by team. */
function byTeam(all: readonly Integration[]): Map<string, Integration[]> {
  const m = new Map<string, Integration[]>();
  for (const in_ of all) {
    const l = m.get(in_.team_id) ?? [];
    l.push(in_);
    m.set(in_.team_id, l);
  }
  return m;
}

const NO_OWNER_RECORD = Symbol("no owner record");

function teamIntakeRecord(ints: Integration[], provider: string, owner: string): Integration | typeof NO_OWNER_RECORD {
  const mine = ints.filter((x) => owner === "" || eqFold(cfg(x, "org"), owner));
  if (mine.length === 0) return NO_OWNER_RECORD;
  try {
    return selectIntegration(mine, provider, "", owner);
  } catch (e) {
    const fac = mine.filter((x) => (x.factory_id ?? "") !== "");
    if (fac.length === 1) return fac[0]!;
    throw e;
  }
}

/**
 * Selects the record for a webhook delivery of provider about one of owners,
 * from all records (all teams): each team gives at most one candidate, and the
 * delivery goes to the one candidate whose secret verifies it.
 */
export async function intakeVerified(
  all: readonly Integration[],
  provider: string,
  owners: readonly string[],
  verify: (in_: Integration) => Promise<boolean>,
): Promise<Integration> {
  const recs = all.filter((x) => eqFold(x.provider, provider));
  const teams = byTeam(recs);
  const cands: Integration[] = [];
  const skipped: string[] = [];
  const seen = new Set<string>();
  for (const owner of owners) {
    for (const [team, ints] of teams) {
      let in_: Integration | typeof NO_OWNER_RECORD;
      try {
        in_ = teamIntakeRecord(ints, provider, owner);
      } catch (e) {
        skipped.push(`team ${team}: ${(e as Error).message}`);
        continue;
      }
      if (in_ === NO_OWNER_RECORD) continue;
      if (!seen.has(in_.id)) {
        seen.add(in_.id);
        cands.push(in_);
      }
    }
  }
  const ok: Integration[] = [];
  for (const in_ of cands) if (await verify(in_)) ok.push(in_);
  if (ok.length === 1) return ok[0]!;
  if (ok.length > 1) {
    const ids = ok.map((x) => `${x.id} (team ${x.team_id})`).join(", ");
    throw new DeliveryAmbiguousError(`${provider}: the records ${ids} verify the delivery: give each team its own webhook secret`);
  }
  if (cands.length === 0 && skipped.length === 0) {
    throw new DeliveryUnverifiedError(`no ${provider} integration record for the owner "${owners.join(", ")}"`, true);
  }
  let msg = `${provider}: ${cands.length} records can serve the owner "${owners.join(", ")}" and none verifies the delivery`;
  if (skipped.length > 0) msg += "; " + skipped.join("; ");
  throw new DeliveryUnverifiedError(msg);
}
