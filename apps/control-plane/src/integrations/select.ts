// The integration selection rule (port of internal/integrations/select.go).
import {
  OfError,
  integrationActive,
  integrationStatus,
  integrationWorkspace,
  integrationWorkspaceKey,
  normalizeWorkspace,
  sameIntegrationProvider,
} from "@openfactory/core";
import type { Integration } from "@openfactory/core";
import type { IntegrationStore } from "./types";

/** No chat or tracker integration record serves an action. */
export class NoIntegrationError extends Error {
  override readonly name = "NoIntegrationError";
}

export function isNoIntegration(e: unknown): boolean {
  return e instanceof NoIntegrationError;
}

function workspaceLabel(provider: string): string {
  if (sameIntegrationProvider(provider, "slack")) return "workspace";
  if (sameIntegrationProvider(provider, "microsoft_teams")) return "Microsoft team";
  if (sameIntegrationProvider(provider, "linear")) return "organization";
  if (sameIntegrationProvider(provider, "jira")) return "site";
  return "workspace";
}

const eqFold = (a: string, b: string): boolean => a.toLowerCase() === b.toLowerCase();

/**
 * Picks the record of factoryId, else the team record, among the records of
 * one team for provider whose workspace is `workspace` (case ignored). A record
 * of another factory is never used. It never returns "the first record".
 * An empty workspace matches each record; then the factory, or else the team,
 * must have exactly one record.
 */
export function select(ints: Integration[], provider: string, factoryId: string, workspaceIn: string): Integration {
  const workspace = normalizeWorkspace(provider, workspaceIn);
  const label = workspaceLabel(provider);
  const tier = (fac: string): Integration[] =>
    ints.filter(
      (i) => (i.factory_id ?? "") === fac && (workspace === "" || eqFold(integrationWorkspace(i), workspace)),
    );
  const teamTier = tier("");
  const tiers = factoryId !== "" ? [tier(factoryId), teamTier] : [teamTier];
  for (const t of tiers) {
    if (t.length === 0) continue;
    if (t.length === 1) return t[0]!;
    const ids = t.map((i) => i.id).join(", ");
    if (workspace === "") {
      throw new NoIntegrationError(
        `${provider}: ${t.length} records (${ids}) can serve this source and it does not name its ${label}: keep one record`,
      );
    }
    throw new NoIntegrationError(
      `${provider}: ${t.length} records (${ids}) serve the ${label} "${workspace}": keep one`,
    );
  }
  if (workspace === "") throw new NoIntegrationError(`no ${provider} integration record serves this source`);
  throw new NoIntegrationError(
    `no ${provider} integration record for the ${label} "${workspace}": add a record with config.${integrationWorkspaceKey(provider)} "${workspace}"`,
  );
}

/** An error for a record that is not active. */
export function inactiveError(i: Integration): OfError | null {
  if (integrationActive(i)) return null;
  let msg = `${i.provider}: integration ${i.id} is ${integrationStatus(i)}`;
  if (i.status_reason) msg += ": " + i.status_reason;
  return new OfError("integration_disabled", msg);
}

/** Loads the records of the team for provider, selects one, and checks that it is active. */
export async function selectTeam(
  store: IntegrationStore | undefined,
  teamId: string,
  provider: string,
  factoryId: string,
  workspace: string,
): Promise<Integration> {
  if (!store) throw new Error("no store configured");
  const all = await store.listIntegrations(teamId);
  const ints = all.filter((i) => sameIntegrationProvider(i.provider, provider));
  const in_ = select(ints, provider, factoryId, workspace);
  const err = inactiveError(in_);
  if (err) throw err;
  return in_;
}

/** A pair, as the Go function returned: a disabled record comes with its error. */
export interface IntakeResult {
  integration: Integration | null;
  error: Error | null;
}

/**
 * Selects the record for an incoming event of provider about workspace, from
 * the records of every team. `match` keeps only the records that can have
 * received the event. The records of exactly one team must be left. In that
 * team the team record wins; a team with no team record uses its record of one
 * factory, when it has exactly one. A selected record that is not active is
 * returned with an integration_disabled error: the caller verifies the event
 * with the secret of the record, then ignores it.
 */
export async function intake(
  store: IntegrationStore | undefined,
  provider: string,
  workspaceIn: string,
  match?: (i: Integration) => boolean,
): Promise<IntakeResult> {
  const fail = (m: string): IntakeResult => ({ integration: null, error: new NoIntegrationError(m) });
  if (!store) return { integration: null, error: new Error("no store configured") };
  const workspace = normalizeWorkspace(provider, workspaceIn);
  const label = workspaceLabel(provider);
  if (workspace === "") return fail(`the ${provider} event does not name its ${label}`);
  const all = await store.listIntegrations("");
  const cands: Integration[] = [];
  const teams = new Set<string>();
  for (const i of all) {
    if (!sameIntegrationProvider(i.provider, provider) || !eqFold(integrationWorkspace(i), workspace)) continue;
    if (match && !match(i)) continue;
    cands.push(i);
    teams.add(i.team_id);
  }
  if (cands.length === 0) return fail(`no ${provider} integration record for the ${label} "${workspace}"`);
  if (teams.size > 1) {
    return fail(
      `records of ${teams.size} teams serve the ${provider} ${label} "${workspace}": keep the records of one team`,
    );
  }
  let first: unknown;
  try {
    const i = select(cands, provider, "", workspace);
    return { integration: i, error: inactiveError(i) };
  } catch (e) {
    first = e;
  }
  const fac = cands.filter((x) => (x.factory_id ?? "") !== "");
  if (fac.length === 1) return { integration: fac[0]!, error: inactiveError(fac[0]!) };
  if (fac.length > 1) {
    return fail(
      `${provider}: ${fac.length} factory records (${fac.map((x) => x.id).join(", ")}) serve the ${label} "${workspace}" and no team record does: add a team record, or keep one`,
    );
  }
  return { integration: null, error: first as Error };
}
