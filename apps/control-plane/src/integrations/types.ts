// Shared contracts of the work-source and chat integrations (Jira, Linear,
// Slack, Teams). Port of the core.Store / core.SecretStore / core.EventBus
// uses of internal/integrations. The Worker wires the real implementations
// (TeamDO RPC, secrets, IngressDO); tests pass fakes.
import type {
  Event,
  ExternalIdentity,
  Integration,
  OutboundMessage,
  PullRequestRef,
  SourceRef,
  User,
} from "@openfactory/core";

export type Fetch = (input: Request | string | URL, init?: RequestInit) => Promise<Response>;

export interface Logger {
  debug(msg: string, fields?: Record<string, unknown>): void;
  info(msg: string, fields?: Record<string, unknown>): void;
  warn(msg: string, fields?: Record<string, unknown>): void;
  error(msg: string, fields?: Record<string, unknown>): void;
}

export const consoleLogger: Logger = {
  debug: () => {},
  info: (m, f) => console.info(m, f ?? ""),
  warn: (m, f) => console.warn(m, f ?? ""),
  error: (m, f) => console.error(m, f ?? ""),
};

/** The slice of the TeamDO state that integrations read and write. */
export interface IntegrationStore {
  /** Records of one team; "" lists the records of every team. Ordered by creation, then ID. */
  listIntegrations(teamId: string): Promise<Integration[]>;
  getIntegration(id: string): Promise<Integration | null>;
  /** Returns true when the status changed. An unknown record is not an error. */
  setIntegrationStatus(id: string, status: string, reason: string): Promise<boolean>;
  findExternalIdentity(provider: string, externalId: string): Promise<ExternalIdentity | null>;
  getUser(id: string): Promise<User | null>;
  getUserByEmail(email: string): Promise<User | null>;
  /** One atomic step. Returns false when the account is linked to another user. */
  linkExternalIdentity(x: ExternalIdentity): Promise<boolean>;
  upsertExternalIdentity(x: ExternalIdentity): Promise<void>;
  /** Factory integration settings by provider (definition `type`). */
  getFactoryIntegrations(factoryId: string): Promise<Record<string, Record<string, string>>>;
  /** Small key-value store (Teams service URLs). */
  kvGet(key: string): Promise<string | null>;
  kvSet(key: string, value: string): Promise<void>;
}

/** Team secrets by name. Throws an OfError "resource_not_found" for an unknown name. */
export interface SecretReader {
  get(teamId: string, name: string): Promise<string>;
}

export interface InboxItemRef {
  id: string;
}

/** What the integrations need from the work side (WorkDO and IndexDO). */
export interface WorkSide {
  /** The newest unresolved inbox item of a work item (approvals and questions first), or null. */
  openInboxItem?(workItemId: string): Promise<InboxItemRef | null>;
  /** The factory of a work item. */
  factoryOf?(workItemId: string): Promise<string>;
  /** Resolves an inbox item when a user presses an option button. actor is the user ID. */
  onAction?(inboxId: string, option: string, actor: string): Promise<void>;
}

export interface LinkerLike {
  privateUrl(teamId: string, provider: string, externalId: string, handle: string): Promise<string>;
  dashboardHint(provider: string): string;
  unlink(provider: string, externalId: string, actorId: string): Promise<void>;
  linkByEmail(teamId: string, c: EmailCandidate): Promise<User | null>;
}

/** An external account that may be linked by its email address. */
export interface EmailCandidate {
  provider: string;
  externalId: string;
  handle: string;
  email: string;
  guest?: boolean;
  foreign?: boolean;
  bot?: boolean;
  deleted?: boolean;
}

export interface IntegrationDeps {
  store: IntegrationStore;
  secrets: SecretReader;
  /** Hands normalized events to ingress (IngressDO). */
  publish: (events: Event[]) => Promise<void>;
  fetch?: Fetch;
  linker?: LinkerLike;
  work?: WorkSide;
  log?: Logger;
  now?: () => Date;
}

/** Posts outbound messages back to a work source. */
export interface Responder {
  provider(): string;
  reply(teamId: string, msg: OutboundMessage): Promise<void>;
}

/** The write-back of a tracker. */
export interface IssueTracker {
  setIssueState(teamId: string, issue: SourceRef, state: string): Promise<void>;
  attachPullRequest(teamId: string, issue: SourceRef, pr: PullRequestRef): Promise<void>;
}

/** Resolves names in automation filters to provider IDs. */
export interface NameResolver {
  resolveNames(
    teamId: string,
    provider: string,
    kind: string,
    names: string[],
    scope?: Record<string, string[]>,
  ): Promise<Record<string, string[]>>;
}

export interface EventContextFile {
  path: string;
  data: Uint8Array;
}

export interface EventContextOmitted {
  name: string;
  reason: string;
}

export interface EventContextResult {
  files: EventContextFile[];
  omitted: EventContextOmitted[];
}

/** Builds the provider files of a run's event context. */
export interface EventContextBuilder {
  eventContext(ev: Event): Promise<EventContextResult>;
}

/** Most bytes of context/thread.json (eventctx.MaxPayloadBytes). */
export const EVENT_CONTEXT_MAX_PAYLOAD_BYTES = 256 << 10;

/**
 * The result of a webhook handler. The response goes back at once. The
 * background step (normalize, enrich, publish) runs after it, for example
 * in ExecutionContext.waitUntil.
 */
export interface WebhookOutcome {
  response: Response;
  background?: () => Promise<void>;
}
