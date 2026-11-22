// Factories, definition versions and the compiled definition with everything
// it declares: agents, harnesses, MCP servers, network policy, skills,
// machines, automations, webhooks and benchmark suites. Port of internal/core
// (definition.go, types.go). Field names are the Go JSON names, so these types
// describe the stored and wire forms.

import type { Timestamp } from "./common";
import type { Scorer } from "./scoring";

export const DEFINITION_SOURCES = ["managed", "git"] as const;
export type DefinitionSource = (typeof DEFINITION_SOURCES)[number];

export interface Factory {
  id: string;
  team_id: string;
  name: string;
  alias: string;
  description?: string;
  source: DefinitionSource;
  git_repo?: string;
  git_branch?: string;
  git_path?: string;
  active_version_id?: string;
  created_at: Timestamp;
  updated_at: Timestamp;
}

export interface ValidationError {
  file: string;
  line?: number;
  field?: string;
  message: string;
}

/** The states of a definition version. */
export const DEFINITION_ACTIVE = "active";
export const DEFINITION_SUPERSEDED = "superseded";
export const DEFINITION_INVALID = "invalid";
export const DEFINITION_PROPOSED = "proposed";
export const DEFINITION_APPROVED = "approved";
export const DEFINITION_REJECTED = "rejected";
export const DEFINITION_STATES = [
  DEFINITION_ACTIVE,
  DEFINITION_SUPERSEDED,
  DEFINITION_INVALID,
  DEFINITION_PROPOSED,
  DEFINITION_APPROVED,
  DEFINITION_REJECTED,
] as const;
export type DefinitionState = (typeof DEFINITION_STATES)[number];

/** One immutable snapshot of a definition tree. */
export interface DefinitionVersion {
  id: string;
  factory_id: string;
  files: Record<string, string>;
  commit_sha?: string;
  author?: string;
  message?: string;
  valid: boolean;
  errors?: ValidationError[];
  applied_at?: Timestamp;
  created_at: Timestamp;
  state?: DefinitionState;
  base_version_id?: string;
  author_run_id?: string;
  inbox_item_id?: string;
  applied_version_id?: string;
  reviewed_by?: string;
  review_comment?: string;
  reviewed_at?: Timestamp;
}

export interface SkippedCheck {
  check: string;
  reason: string;
}

/** The result of the compiler and the preflight for one definition tree. */
export interface DefinitionCheck {
  errors: ValidationError[];
  warnings: ValidationError[];
  skipped: SkippedCheck[];
}

/** The code forges a repository can live on. */
export const CODE_FORGES = ["GITHUB", "GITLAB", "AZURE_DEVOPS"] as const;
export type CodeForge = (typeof CODE_FORGES)[number];

/** A repository a factory is scoped to. */
export interface RepoRef {
  codeForge: CodeForge;
  owner: string;
  name: string;
}

export interface RepoAtCommit {
  codeForge: CodeForge;
  owner: string;
  repo: string;
  /** 40-char sha */
  ref: string;
}

export interface RepoSubstitution {
  source: RepoRef;
  target: RepoRef;
}

/** The integrations a definition can declare. */
export const INTEGRATION_DECL_TYPES = ["slack", "microsoft-teams", "linear", "jira"] as const;
export type IntegrationDeclType = (typeof INTEGRATION_DECL_TYPES)[number];

export interface IntegrationDecl {
  type: IntegrationDeclType;
  settings?: Record<string, string>;
}

/** Who reviews the pull requests of self-improvement runs. */
export const SELF_IMPROVEMENT_REVIEWER_TYPES = ["admins", "team", "custom", "none"] as const;
export type SelfImprovementReviewerType = (typeof SELF_IMPROVEMENT_REVIEWER_TYPES)[number];

export interface SelfImprovementConfig {
  failedRunThreshold?: number;
  reviewerType?: SelfImprovementReviewerType;
  reviewerEmails?: string[];
  analysisModel?: string;
}

export interface CloudProviders {
  gcp?: {
    projectNumber: string;
    workloadIdentityFederationPoolId: string;
    workloadIdentityFederationProviderId: string;
    serviceAccountEmail?: string;
  };
  audiences?: string[];
}

/** A limit on model spend in US dollars. 0 means no limit for that period. */
export interface SpendLimit {
  daily?: number;
  monthly?: number;
}

export interface BenchmarkAgentPin {
  agent: string;
  model?: string;
  harness?: HarnessConfig;
}

export interface BenchmarkConfiguration {
  name: string;
  agents?: BenchmarkAgentPin[];
  machine?: string;
}

export interface BenchmarkTask {
  slug: string;
  title: string;
  prompt: string;
  successCriteria: string;
  sourceRunId?: string;
  tags?: string[];
  trackerSeed?: unknown;
  startingRepoRefs?: RepoAtCommit[];
}

export interface BenchmarkSuite {
  slug: string;
  name: string;
  description?: string;
  agent: string;
  configurations?: BenchmarkConfiguration[];
  tasks?: BenchmarkTask[];
}

/** Whose forge credentials a run uses: the person who starts it, or the creator of the factory. */
export const CREDENTIAL_STRATEGIES = ["EXECUTOR", "CREATOR"] as const;
export type CredentialStrategy = (typeof CREDENTIAL_STRATEGIES)[number];

/** The validated, resolved form of a definition tree. */
export interface CompiledDefinition {
  schemaVersion: string;
  name: string;
  description?: string;
  alias?: string;
  credentialStrategy?: CredentialStrategy;
  codeForges?: CodeForge[];
  repositories: RepoRef[] | null;
  integrations?: IntegrationDecl[];
  integrationsSet: boolean;
  secrets?: string[];
  mcpServers?: Record<string, MCPServer>;
  cloudProviders?: CloudProviders;
  selfImprovement?: SelfImprovementConfig;
  githubOnboardingNotices: boolean;
  factoryConfigChecks?: string;
  agents: AgentConfig[] | null;
  automations: Automation[] | null;
  machines: Record<string, MachineSpec> | null;
  scorers: Scorer[] | null;
  scorerDefaults?: AgentOverrides;
  webhooks: Webhook[] | null;
  benchmarks: BenchmarkSuite[] | null;
  benchmarkRepoSubstitutions?: RepoSubstitution[];
  skills: Skill[] | null;
  maxConcurrentRuns?: number;
  allowAgentMerge?: boolean;
  spendLimit?: SpendLimit;
  approvers?: string[];
  restrictFollowupsToStarter?: boolean;
  requireLinkedActor?: boolean;
}

// ---------- Agents ----------

export const AGENT_TYPES = ["FOREMAN", "TRIAGE", "SPEC", "IMPLEMENT", "REVIEW", "VERIFY", "CUSTOM"] as const;
export type AgentType = (typeof AGENT_TYPES)[number];

/** The harness types of a compiled definition. Empty means the default harness for the model. */
export const HARNESS_TYPES = ["claude", "codex", ""] as const;
export type HarnessType = (typeof HARNESS_TYPES)[number];

/** Where a harness gets its model credential. */
export const HARNESS_AUTH_SOURCES = ["managedSecret", "workerEnvironment"] as const;
export type HarnessAuthSource = (typeof HARNESS_AUTH_SOURCES)[number];

export interface HarnessAuth {
  source: HarnessAuthSource;
  secretName?: string;
}

export interface HarnessConfig {
  type: HarnessType;
  model: string;
  reasoningLevel?: string;
  auth?: HarnessAuth;
}

/** One MCP server transport. Exactly one of managedId, command and url is set. */
export interface MCPServer {
  managedId?: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
  allowWrite?: boolean;
}

/** The managedId namespace of the MCP servers that the control plane serves itself. */
export const BUILTIN_MCP_PREFIX = "builtin/";
/** The integrations with a built-in MCP server. */
export const BUILTIN_MCP_PROVIDERS: readonly string[] = ["jira", "linear", "slack", "teams"];

export interface NetworkPolicy {
  extends?: string[];
  allowedHosts?: string[];
  deniedHosts?: string[];
}

/** The merged result. restricted=false means unrestricted. */
export interface EffectiveNetworkPolicy {
  restricted: boolean;
  allowed_hosts: string[] | null;
  denied_hosts: string[] | null;
}

export interface Skill {
  name: string;
  description: string;
  /** path inside the definition tree */
  path: string;
  /** full SKILL.md */
  content: string;
  /** supporting files, relative to the skill directory */
  files?: Record<string, string>;
  /** "factory", "repository", "builtin" or the agent name */
  scope: string;
}

export interface MachineSpec {
  name: string;
  description?: string;
  setupCommands?: string[];
  vcpus?: number;
  memoryGb?: number;
  os: string;
  arch: string;
  image?: string;
  macVersion?: string;
  registryCredentialSecretName?: string;
  user?: string;
  failureSessionRetentionMinutes?: number;
}

/** The fully resolved configuration of one agent. */
export interface AgentConfig {
  id: string;
  factory_id: string;
  name: string;
  description: string;
  type: AgentType;
  instructions: string;
  harness: HarnessConfig;
  machine?: MachineSpec;
  environment_id?: string;
  worker_host?: string;
  secrets?: string[];
  mcp_servers?: Record<string, MCPServer>;
  network_policy: EffectiveNetworkPolicy;
  /** null or undefined: foreman only */
  spawnable_by: string[] | null;
  idle_timeout_minutes?: number;
  idle_timeout_set?: boolean;
  /** Empty: the factory's strategy. The JSON form omits it. */
  credential_strategy?: CredentialStrategy | "";
  skills?: Skill[];
  snapshot?: boolean;
}

// ---------- Automations ----------

/** Values of Trigger.concurrency. */
export const SCHEDULE_CONCURRENCY_ALLOW = "allow";
export const SCHEDULE_CONCURRENCY_SKIP = "skip";
export const SCHEDULE_CONCURRENCIES = [SCHEDULE_CONCURRENCY_ALLOW, SCHEDULE_CONCURRENCY_SKIP] as const;
export type ScheduleConcurrency = (typeof SCHEDULE_CONCURRENCIES)[number];

export interface Schedule {
  name?: string;
  cron: string;
}

export interface Trigger {
  /** github, gitlab, azure_devops, slack, microsoft_teams, linear, jira, webhook, schedule */
  provider: string;
  event: string;
  filter?: unknown;
  schedule?: Schedule;
  concurrency?: ScheduleConcurrency;
}

/** Sparse execution overrides (automation level or run_agents level). */
export interface AgentOverrides {
  harness?: HarnessConfig;
  model?: string;
  machine?: string;
  environment_id?: string;
  secrets: string[] | null;
  mcp_servers: Record<string, MCPServer> | null;
  worker_host?: string;
  idle_timeout_minutes?: number;
}

export interface Automation {
  id: string;
  factory_id: string;
  name: string;
  display_name?: string;
  enabled: boolean;
  agent: string;
  prompt: string;
  triggers: Trigger[] | null;
  run_as?: string;
  overrides?: AgentOverrides;
}

// ---------- Webhooks ----------

export const WEBHOOK_AUTH_MODES = ["token", "url_token", "signature"] as const;
export type WebhookAuthMode = (typeof WEBHOOK_AUTH_MODES)[number];

/** The provider signature schemes a webhook can verify. */
export const WEBHOOK_SIGNATURE_SCHEMES = [
  "github",
  "pagerduty",
  "sentry",
  "standard_webhooks",
  "stripe",
  "vercel",
] as const;
export type WebhookSignatureScheme = (typeof WEBHOOK_SIGNATURE_SCHEMES)[number];

export interface Webhook {
  id: string;
  factory_id: string;
  name: string;
  auth_mode: WebhookAuthMode;
  signature_scheme?: WebhookSignatureScheme;
  secret_name: string;
  delivery_id_header?: string;
  enabled: boolean;
  created_at: Timestamp;
}

// ---------- Functions ----------

/** The state of a definition version. activeId is the active version of the factory. */
export function definitionVersionState(
  v: Pick<DefinitionVersion, "state" | "valid" | "id">,
  activeId: string,
): DefinitionState {
  if (v.state) return v.state;
  if (!v.valid) return DEFINITION_INVALID;
  if (v.id === activeId) return DEFINITION_ACTIVE;
  return DEFINITION_SUPERSEDED;
}

/** "owner/name" of a repository. */
export function repoFullName(r: Pick<RepoRef, "owner" | "name">): string {
  return r.owner + "/" + r.name;
}
