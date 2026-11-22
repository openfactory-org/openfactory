// Compiled definition types. Each type is the JSON shape of its Go struct in
// internal/core (definition.go, types.go): the same field names, and an
// optional field where Go uses `omitempty`. compile() returns values in this
// shape, so the JSON that the control plane stores equals the JSON of the Go
// server. The one difference: Webhook has no created_at (the store sets it).

export interface ValidationError {
  file: string;
  /** 1-based line; absent when unknown (Go omits 0). */
  line?: number;
  field?: string;
  message: string;
}

export type HarnessType = "claude" | "codex" | "";

export type AgentType = "FOREMAN" | "TRIAGE" | "SPEC" | "IMPLEMENT" | "REVIEW" | "VERIFY" | "CUSTOM";

export interface HarnessAuth {
  /** managedSecret | workerEnvironment */
  source: string;
  secretName?: string;
}

export interface HarnessConfig {
  type: HarnessType;
  model: string;
  reasoningLevel?: string;
  auth?: HarnessAuth;
}

/** MCPServer is one MCP server transport: exactly one of managedId, command and url. */
export interface MCPServer {
  managedId?: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
  allowWrite?: boolean;
}

export interface NetworkPolicy {
  extends?: string[];
  allowedHosts?: string[];
  deniedHosts?: string[];
}

export interface EffectiveNetworkPolicy {
  restricted: boolean;
  allowed_hosts: string[] | null;
  denied_hosts: string[] | null;
}

export interface Skill {
  name: string;
  description: string;
  path: string;
  content: string;
  files?: Record<string, string>;
  /** "factory" or the agent name. */
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
  /** null: only the foreman can start the agent. */
  spawnable_by: string[] | null;
  idle_timeout_minutes?: number;
  idle_timeout_set?: boolean;
  credential_strategy?: string;
  skills?: Skill[];
  snapshot?: boolean;
}

export interface Schedule {
  name?: string;
  cron: string;
}

export interface Trigger {
  provider: string;
  event: string;
  /** JSON filter (Go json.RawMessage). */
  filter?: unknown;
  schedule?: Schedule;
  concurrency?: string;
}

export interface AgentOverrides {
  harness?: HarnessConfig;
  model?: string;
  machine?: string;
  environment_id?: string;
  /** Not omitempty in Go: null keeps the agent's list, [] clears it. */
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

export interface Webhook {
  id: string;
  factory_id: string;
  name: string;
  auth_mode: string;
  signature_scheme?: string;
  secret_name: string;
  delivery_id_header?: string;
  enabled: boolean;
}

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

export interface RepoRef {
  codeForge: string;
  owner: string;
  name: string;
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

export interface RepoAtCommit {
  codeForge: string;
  owner: string;
  repo: string;
  ref: string;
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

export interface GCPProvider {
  projectNumber: string;
  workloadIdentityFederationPoolId: string;
  workloadIdentityFederationProviderId: string;
  serviceAccountEmail?: string;
}

export interface CloudProviders {
  gcp?: GCPProvider;
  audiences?: string[];
}

export interface SpendLimit {
  daily?: number;
  monthly?: number;
}

export interface CompiledDefinition {
  schemaVersion: string;
  name: string;
  description?: string;
  alias?: string;
  credentialStrategy?: string;
  codeForges?: string[];
  repositories: RepoRef[] | null;
  integrations?: IntegrationDecl[];
  /** false: keep the factory's existing integrations. */
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

export interface CompileResult {
  /** The compiled definition; null when there are errors. */
  definition: CompiledDefinition | null;
  /** Validation errors sorted by file, then line. */
  errors: ValidationError[];
}
