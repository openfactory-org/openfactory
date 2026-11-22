// The integration selection rule. Port of internal/forge/forgeutil/select.go.
import {
  OfError,
  intakeVerified as intakeRecord,
  ownerRule,
  selectRecord,
  type Integration,
  type RecordRule,
} from "@openfactory/core";

export { repoOwner } from "@openfactory/core";

/** reason of the OfError (invalid_request) when no record serves the owner (Go ErrNoIntegration). */
export const REASON_NO_INTEGRATION = "no_integration";

export function noIntegration(msg: string): OfError {
  return new OfError("invalid_request", `${msg}: no code-forge integration configured`, {
    reason: REASON_NO_INTEGRATION,
  });
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

const rule = (provider: string): RecordRule<Integration> =>
  ownerRule(
    provider,
    {
      id: (r) => r.id,
      team: (r) => r.team_id,
      factory: (r) => r.factory_id ?? "",
      org: (r) => r.config?.["org"] ?? "",
    },
    noIntegration,
  );

/**
 * Selects one record of ints (one provider of one team) for factoryId and
 * owner: the records whose config.org is owner (case ignored), the factory
 * record first, else the team record. Never "the first record".
 */
export function selectIntegration(
  ints: readonly Integration[],
  provider: string,
  factoryId: string,
  owner: string,
): Integration {
  return selectRecord(ints, rule(provider), factoryId, owner);
}

/**
 * Selects the record for a webhook delivery of provider about one of owners,
 * from all records (all teams): each team gives at most one candidate, and the
 * delivery goes to the one candidate whose secret verifies it.
 */
export function intakeVerified(
  all: readonly Integration[],
  provider: string,
  owners: readonly string[],
  verify: (in_: Integration) => Promise<boolean>,
): Promise<Integration> {
  const recs = all.filter((x) => x.provider.toLowerCase() === provider.toLowerCase());
  return intakeRecord(recs, rule(provider), provider, owners, verify, {
    ambiguous: (msg) => new DeliveryAmbiguousError(msg),
    unverified: (msg, noRecord) => new DeliveryUnverifiedError(msg, noRecord),
  });
}
