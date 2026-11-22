// Domain types. Port of the non-runtime parts of internal/core (types.go,
// principal.go, references.go, source.go, definition.go, attachment.go,
// prfacts.go, reviewcontext.go, dispatch.go, coordination.go, scoring.go,
// turnend.go, interfaces.go). Field names are the Go JSON names, so these
// types describe the stored and wire forms. Times are RFC 3339 strings.

import type { FailureCode } from "./failure";

/** An RFC 3339 time string. */
export type Timestamp = string;

// ---------- Identity ----------

export interface Team {
  id: string;
  name: string;
  created_at: Timestamp;
}

export type Role = "admin" | "editor" | "user" | (string & {});

export const ROLE_ADMIN = "admin";
export const ROLE_EDITOR = "editor";
/** The lowest role: it can view everything and run work. */
export const ROLE_USER = "user";

/** UserKind values of User.kind. A person has an empty kind. */
export const USER_KIND_PERSON = "";
export const USER_KIND_SERVICE_ACCOUNT = "service_account";
/** The ID prefix of a service account. */
export const SERVICE_ACCOUNT_ID_PREFIX = "sa";

export interface User {
  id: string;
  team_id: string;
  email: string;
  name: string;
  role: Role;
  /** Role overrides for single factories, keyed by factory ID. */
  factory_roles?: Record<string, Role>;
  /** USER_KIND_PERSON (empty) or USER_KIND_SERVICE_ACCOUNT. */
  kind?: string;
  created_at: Timestamp;
}

/** Links a user to an account on an integration. */
export interface ExternalIdentity {
  user_id: string;
  provider: string;
  external_id: string;
  handle: string;
}

export interface APIKey {
  id: string;
  team_id: string;
  user_id: string;
  name: string;
  prefix: string;
  created_at: Timestamp;
  last_used_at?: Timestamp;
}

export type PrincipalType = "user" | "service_account" | "factory";

/** Who an action is done for. A run records its creator and its run_as. */
export interface Principal {
  type: PrincipalType;
  id: string;
  name?: string;
}

/** Values of Automation.run_as. */
export const RUN_AS_FACTORY = "factory";
export const RUN_AS_USER_PREFIX = "user:";
export const RUN_AS_SERVICE_ACCOUNT_PREFIX = "serviceAccount:";

// ---------- Factory and definitions ----------

export type DefinitionSource = "managed" | "git";

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
  state?: string;
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

/** A repository a factory is scoped to. codeForge: GITHUB, GITLAB, AZURE_DEVOPS. */
export interface RepoRef {
  codeForge: string;
  owner: string;
  name: string;
}

export interface RepoAtCommit {
  codeForge: string;
  owner: string;
  repo: string;
  /** 40-char sha */
  ref: string;
}

export interface RepoSubstitution {
  source: RepoRef;
  target: RepoRef;
}

export interface IntegrationDecl {
  type: string;
  settings?: Record<string, string>;
}

export interface SelfImprovementConfig {
  failedRunThreshold?: number;
  reviewerType?: string;
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

/** The validated, resolved form of a definition tree. */
export interface CompiledDefinition {
  schemaVersion: string;
  name: string;
  description?: string;
  alias?: string;
  credentialStrategy?: string;
  codeForges?: string[];
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

export type AgentType = "FOREMAN" | "TRIAGE" | "SPEC" | "IMPLEMENT" | "REVIEW" | "VERIFY" | "CUSTOM";

export const AGENT_TYPES: readonly AgentType[] = [
  "FOREMAN",
  "TRIAGE",
  "SPEC",
  "IMPLEMENT",
  "REVIEW",
  "VERIFY",
  "CUSTOM",
];

export type HarnessType = "claude" | "codex" | "";

export interface HarnessAuth {
  /** managedSecret | workerEnvironment */
  source: string;
  secretName?: string;
}

export interface HarnessConfig {
  type: HarnessType | string;
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

/** Values of RegisteredMCPServer.connection. */
export const MCP_CONNECTION_PROXY = "proxy";
export const MCP_CONNECTION_DIRECT = "direct";

/** MCP OAuth connection states. */
export const MCP_OAUTH_NOT_CONNECTED = "not_connected";
export const MCP_OAUTH_CONNECTED = "connected";
export const MCP_OAUTH_ERROR = "error";

export interface MCPOAuthConfig {
  scopes?: string[];
  client_id?: string;
  client_secret_name?: string;
  authorization_endpoint?: string;
  token_endpoint?: string;
  status: string;
  connected_by?: string;
  connected_at?: Timestamp;
  error?: string;
}

/** An entry of a team's MCP server registry. */
export interface RegisteredMCPServer {
  id: string;
  team_id: string;
  name: string;
  description?: string;
  url?: string;
  headers?: Record<string, string>;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  connection?: string;
  oauth?: MCPOAuthConfig;
  created_by?: string;
  created_at: Timestamp;
  updated_at: Timestamp;
}

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
  /** EXECUTOR | CREATOR */
  credential_strategy?: string;
  skills?: Skill[];
  snapshot?: boolean;
}

// ---------- Automations ----------

/** Values of Trigger.concurrency. */
export const SCHEDULE_CONCURRENCY_ALLOW = "allow";
export const SCHEDULE_CONCURRENCY_SKIP = "skip";

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
  concurrency?: string;
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

// ---------- Events ----------

/** Sender trust values (Actor.trust). */
export const TRUST_MEMBER = "member";
export const TRUST_EXTERNAL = "external";
export const TRUST_PLATFORM = "platform";

export interface Actor {
  provider: string;
  external_id: string;
  handle: string;
  is_bot: boolean;
  user_id?: string;
  is_self?: boolean;
  trust?: string;
}

/** Where a work item came from, so that results return there. */
export interface SourceRef {
  provider: string;
  kind: string;
  url?: string;
  repo?: string;
  number?: number;
  channel?: string;
  thread_id?: string;
  external_id?: string;
  title?: string;
  /** A sub-thread of the source. Not part of the key. */
  thread_ref?: string;
  /** The workspace, team or site of a chat or tracker source. Not part of the key. */
  workspace?: string;
}

/** A normalized inbound event from any work source. */
export interface Event {
  id: string;
  team_id: string;
  factory_id?: string;
  provider: string;
  type: string;
  delivery_id?: string;
  action_key?: string;
  actor: Actor;
  source: SourceRef;
  attributes: Record<string, unknown> | null;
  payload?: unknown;
  text?: string;
  received_at: Timestamp;
}

// ---------- Work items ----------

export type Stage =
  | "intake"
  | "triage"
  | "planning"
  | "awaiting_approval"
  | "building"
  | "reviewing"
  | "handoff"
  | "complete"
  | "cancelled";

export const STAGES: readonly Stage[] = [
  "intake",
  "triage",
  "planning",
  "awaiting_approval",
  "building",
  "reviewing",
  "handoff",
  "complete",
  "cancelled",
];

export interface PullRequestRef {
  forge: string;
  repo: string;
  number: number;
  url: string;
  branch: string;
  /** open, draft, merged, closed */
  state: string;
}

/** Kinds of a work item reference. */
export const REF_SOURCE = "source";
export const REF_PULL_REQUEST = "pull_request";
export const REF_ISSUE = "issue";
export const REF_THREAD = "thread";

/** One external reference of a work item. Its key routes events to the work item. */
export interface WorkItemRef {
  kind: string;
  key: string;
  source: SourceRef;
  added_at?: Timestamp;
}

export interface WorkItem {
  id: string;
  factory_id: string;
  title: string;
  stage: Stage;
  source: SourceRef;
  source_key?: string;
  automation_id?: string;
  creator_id?: string;
  root_run_id?: string;
  pull_requests?: PullRequestRef[];
  references?: WorkItemRef[];
  labels?: Record<string, string>;
  local_owner?: string;
  created_at: Timestamp;
  updated_at: Timestamp;
  completed_at?: Timestamp;
  version: number;
}

/** The timeline of a work item. */
export interface WorkItemEvent {
  id: string;
  work_item_id: string;
  kind: string;
  run_id?: string;
  actor?: string;
  data?: unknown;
  created_at: Timestamp;
}

/** Issue states that the engine writes back to a tracker. */
export const ISSUE_STATE_IN_PROGRESS = "in_progress";
export const ISSUE_STATE_IN_REVIEW = "in_review";
export const ISSUE_STATE_DONE = "done";

/** Outbound message kinds that are tracker operations, not text. */
export const OUTBOUND_ISSUE_STATE = "issue_state";
export const OUTBOUND_ATTACH_PULL_REQUEST = "attach_pull_request";

/** Something the factory says back to a work source. */
export interface OutboundMessage {
  work_item_id: string;
  source: SourceRef;
  /** markdown */
  text: string;
  /** update, question, approval_request, handoff, error, issue_state, attach_pull_request */
  kind: string;
  options?: string[];
  inbox_id?: string;
  factory_id?: string;
  thread_ref?: string;
  issue_state?: string;
  pull_request?: PullRequestRef;
}

// ---------- Runs ----------

export type RunState =
  | "queued"
  | "pending"
  | "claimed"
  | "running"
  | "blocked"
  | "succeeded"
  | "failed"
  | "error"
  | "cancelled"
  /** Old states that only older stored runs carry. */
  | "starting"
  | "idle";

/** The closed list of run sources of the API. */
export type RunSource =
  | "API"
  | "MCP"
  | "DASHBOARD"
  | "CLI"
  | "AUTOMATION"
  | "SCHEDULE"
  | "WEBHOOK"
  | "MENTION"
  | "ORCHESTRATION"
  | "SCORER"
  | "BENCHMARK"
  | "SELF_IMPROVEMENT";

/** Every RunSource in the order of the API. */
export const RUN_SOURCES: readonly RunSource[] = [
  "API",
  "MCP",
  "DASHBOARD",
  "CLI",
  "AUTOMATION",
  "SCHEDULE",
  "WEBHOOK",
  "MENTION",
  "ORCHESTRATION",
  "SCORER",
  "BENCHMARK",
  "SELF_IMPROVEMENT",
];

/** Usage coverage values. */
export const USAGE_KNOWN = "known";
export const USAGE_PARTIAL = "partial";

/** Token, cost and time accounting of a run, a session or a turn. */
export interface Usage {
  input_tokens: number;
  output_tokens: number;
  cost_usd: number;
  duration_ms: number;
  coverage?: string;
}

/** The queued reason of a run that waits because spend is at the limit. */
export const QUEUED_SPEND_LIMIT = "spend_limit";

export interface Run {
  id: string;
  version: number;
  team_id: string;
  factory_id: string;
  work_item_id?: string;
  parent_run_id?: string;
  agent_id?: string;
  agent_name?: string;
  name?: string;
  state: RunState;
  prompt: string;
  config: AgentConfig;
  metadata?: Record<string, string>;
  session_id?: string;
  consumed_seq: number;
  sandbox_id?: string;
  worker_id?: string;
  result?: string;
  error?: string;
  failure_code?: FailureCode;
  blocked_reason?: string;
  queued_reason?: string;
  turns?: number;
  cancelled_by?: string;
  usage: Usage;
  triggered_by?: string;
  source?: RunSource;
  source_ref?: string;
  creator?: Principal;
  run_as?: Principal;
  created_at: Timestamp;
  started_at?: Timestamp;
  finished_at?: Timestamp;
  queued_at?: Timestamp;
}

/** One entry in a run's transcript. role: user, assistant, tool_call, tool_result, system, log. */
export interface RunMessage {
  id: string;
  run_id: string;
  seq: number;
  role: string;
  content: string;
  data?: unknown;
  created_at: Timestamp;
}

/** Outcomes of a message to a run. */
export const MESSAGE_LIVE_SESSION = "live_session";
export const MESSAGE_NEW_SESSION = "new_session";
export const MESSAGE_QUEUED = "queued";

export interface MessageReceipt {
  message_id: string;
  outcome: string;
}

export interface ChildRunConfig {
  name: string;
  agent_identity_uid: string;
  prompt: string;
  metadata?: Record<string, string>;
  overrides?: AgentOverrides;
  skills?: string[];
}

export interface RunAgentsRequest {
  summary: string;
  base_prompt: string;
  remote?: Record<string, unknown>;
  metadata?: Record<string, string>;
  agent_run_configs: ChildRunConfig[] | null;
}

/** The run metadata key that puts a run in trial mode. */
export const META_TRIAL_MODE = "trial_mode";

/** The start commit of one repository of a run. */
export interface RepoStart {
  codeForge: string;
  owner: string;
  repo: string;
  path?: string;
  ref: string;
  resolved?: boolean;
}

// ---------- Inbox ----------

export type InboxKind =
  "approval" | "question" | "handoff" | "failure" | "review" | "spend_limit" | "blocked" | "answered";

/** Question types of an inbox question. */
export const QUESTION_SINGLE_SELECT = "single_select";
export const QUESTION_MULTI_SELECT = "multi_select";
export const QUESTION_TEXT = "text";

export interface Question {
  question_id: string;
  text: string;
  type: string;
  options?: string[];
  recommended_option?: string;
}

/** Tools an inbox item can be resolved from (InboxItem.resolved_via). */
export const INBOX_VIA_DASHBOARD = "dashboard";
export const INBOX_VIA_API = "api";
export const INBOX_VIA_MCP = "mcp";
export const INBOX_VIA_CHAT = "chat";
export const INBOX_VIA_FORGE = "forge";

export interface InboxItem {
  id: string;
  factory_id: string;
  work_item_id?: string;
  run_id?: string;
  kind: InboxKind;
  title: string;
  body: string;
  options?: string[];
  questions?: Question[];
  assignee_id?: string;
  recipients?: string[];
  read_by?: string[];
  answer_message_id?: string;
  resolved: boolean;
  resolution?: string;
  resolved_by?: string;
  resolved_via?: string;
  created_at: Timestamp;
  resolved_at?: Timestamp;
  version: number;
}

/** Who resolves an inbox item and from which tool. */
export interface Resolver {
  userId: string;
  via: string;
}

// ---------- Attachments ----------

/** The size limit of one uploaded attachment. */
export const MAX_ATTACHMENT_BYTES = 25 << 20;
/** The most attachments one message can carry. */
export const MAX_ATTACHMENTS_PER_MESSAGE = 20;

export interface Attachment {
  attachment_id: string;
  team_id: string;
  name: string;
  content_type: string;
  bytes: number;
  created_by?: string;
  created_at: Timestamp;
}

// ---------- Secrets ----------

export type SecretType =
  "generic" | "anthropic_api_key" | "anthropic_oauth_token" | "openai_api_key" | "docker_registry" | (string & {});

export interface Secret {
  id: string;
  team_id: string;
  factory_id?: string;
  name: string;
  type: SecretType;
  created_at: Timestamp;
  updated_at: Timestamp;
}

// ---------- Webhooks ----------

export interface Webhook {
  id: string;
  factory_id: string;
  name: string;
  /** token | url_token | signature */
  auth_mode: string;
  signature_scheme?: string;
  secret_name: string;
  delivery_id_header?: string;
  enabled: boolean;
  created_at: Timestamp;
}

/** The status of a delivery log entry for a repeated delivery. */
export const WEBHOOK_DELIVERY_DEDUPLICATED = "deduplicated";

export interface WebhookDelivery {
  id: string;
  webhook_id: string;
  delivery_id?: string;
  /** accepted, rejected, deduplicated */
  status: string;
  reason?: string;
  headers?: Record<string, string>;
  body?: unknown;
  run_ids?: string[];
  received_at: Timestamp;
}

// ---------- Integrations ----------

/** Integration statuses. */
export const INTEGRATION_ACTIVE = "active";
export const INTEGRATION_DISABLED = "disabled";
export const INTEGRATION_AUTH_FAILED = "auth_failed";

export interface Integration {
  id: string;
  team_id: string;
  provider: string;
  name: string;
  config: Record<string, string> | null;
  secret_ref?: string;
  factory_id?: string;
  status?: string;
  status_reason?: string;
  status_updated_at?: Timestamp;
  created_at: Timestamp;
}

// ---------- Scoring and benchmarks ----------

export interface ScorerLabel {
  value: string;
  description?: string;
  score: number;
}

export interface ScorerAgent {
  name: string;
  includeDescendants?: boolean;
}

export interface Scorer {
  id: string;
  factory_id: string;
  slug: string;
  name: string;
  description?: string;
  agents: ScorerAgent[] | null;
  labels: ScorerLabel[] | null;
  passing_score: number;
  sampling_rate: number;
  model: string;
  rubric: string;
  self_improvement: boolean;
  machine?: string;
  secrets?: string[];
  mcp_servers?: Record<string, MCPServer>;
  harness?: HarnessConfig;
}

export interface Score {
  id: string;
  scorer_id: string;
  run_id: string;
  agent_name: string;
  label: string;
  score: number;
  passed: boolean;
  rationale: string;
  created_at: Timestamp;
  improvement_run_id?: string;
  judge_run_id?: string;
  judge_usage?: Usage;
}

/** The name of the skill that holds the files of a judge run. */
export const JUDGE_SKILL_NAME = "scored-run";

export interface BenchmarkRun {
  id: string;
  factory_id: string;
  suite: string;
  /** running, complete, failed, cancelled */
  state: string;
  results?: unknown;
  created_at: Timestamp;
  finished_at?: Timestamp;
}

// ---------- Workers and environments ----------

export interface Worker {
  id: string;
  team_id: string;
  name: string;
  backend: string;
  labels?: Record<string, string>;
  capacity: number;
  active: number;
  last_heartbeat: Timestamp;
  version?: string;
}

/** A reusable workspace (repositories and setup) that agents run in. */
export interface Environment {
  id: string;
  team_id: string;
  name: string;
  description?: string;
  /** forge:owner/name */
  repos: string[] | null;
  image?: string;
  setup_commands?: string[];
  env_vars?: Record<string, string>;
}

// ---------- Pull request facts ----------

/** Actor kinds of a pull request push. */
export const PUSH_BY_AGENT = "agent";
export const PUSH_BY_BOT = "bot";
export const PUSH_BY_PERSON = "person";

export interface PullRequestPush {
  at: Timestamp;
  actor?: string;
  kind: string;
  sha?: string;
}

export interface PullRequestFacts {
  /** team_id + "|" + key */
  id: string;
  team_id: string;
  key: string;
  provider: string;
  repo: string;
  number: number;
  url?: string;
  head_branch?: string;
  opener?: string;
  opener_kind?: string;
  opened_at?: Timestamp;
  pushes?: PullRequestPush[];
  first_review_at?: Timestamp;
  merged_at?: Timestamp;
  closed_at?: Timestamp;
  additions?: number;
  deletions?: number;
  updated_at: Timestamp;
}

export interface PullRequestFactFilter {
  teamId?: string;
  keys?: string[];
  provider?: string;
  /** case-insensitive */
  repo?: string;
  headBranch?: string;
  openOnly?: boolean;
}

// ---------- Review context ----------

export interface ThreadComment {
  id?: string;
  author?: string;
  body?: string;
  path?: string;
  line?: number;
  created_at?: Timestamp;
}

export interface ReviewThread {
  ref?: string;
  path?: string;
  line?: number;
  resolved?: boolean;
  comments: ThreadComment[] | null;
}

export interface FileDiff {
  path: string;
  patch: string;
  truncated?: boolean;
}

/** Review events of a forge review. */
export const REVIEW_EVENT_COMMENT = "comment";
export const REVIEW_EVENT_APPROVE = "approve";
export const REVIEW_EVENT_REQUEST_CHANGES = "request_changes";

export interface ReviewComment {
  path: string;
  line: number;
  side?: string;
  body: string;
}

// ---------- Dispatch and delivery ----------

/** One dispatch decision: one action given to one target ("auto:<id>", "wi:<id>", "mention:<factory>"). */
export interface DispatchRecord {
  action_key: string;
  target: string;
  ref?: string;
  created_at: Timestamp;
}

/** How long dispatch records are kept, in milliseconds. */
export const DISPATCH_RETENTION_MS = 7 * 24 * 60 * 60 * 1000;

/** Topics. The durable ones go through an outbox. */
export const TOPIC_INBOUND_EVENT = "event.inbound";
export const TOPIC_RUN_UPDATED = "run.updated";
export const TOPIC_RUN_MESSAGE = "run.message";
export const TOPIC_WORK_ITEM = "workitem.updated";
export const TOPIC_INBOX = "inbox.updated";
export const TOPIC_DEFINITION = "definition.applied";
export const TOPIC_OUTBOUND_NOTICE = "notice.outbound";
export const DURABLE_TOPICS: readonly string[] = [TOPIC_INBOUND_EVENT, TOPIC_OUTBOUND_NOTICE];

export type OutboxState = "pending" | "delivered" | "dead";

export interface ListOpts {
  limit?: number;
  cursor?: string;
  ascending?: boolean;
}

export interface RunFilter extends ListOpts {
  teamId?: string;
  factoryId?: string;
  workItemId?: string;
  parentRunId?: string;
  agentName?: string;
  name?: string;
  creatorId?: string;
  states?: RunState[];
  since?: Timestamp;
}

export interface WorkItemFilter extends ListOpts {
  factoryId?: string;
  stages?: Stage[];
  creatorId?: string;
  search?: string;
  since?: Timestamp;
}

/** The list page shape of every list endpoint. */
export interface PageInfo {
  has_next_page: boolean;
  next_cursor?: string;
}

export interface ListPage<T> {
  items: T[];
  page_info: PageInfo;
}
