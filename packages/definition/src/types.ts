// Compiled definition types. The shared shapes live in @openfactory/core; each
// is the JSON shape of its Go struct in internal/core (definition.go,
// types.go). compile() returns values in this shape, so the JSON that the
// control plane stores equals the JSON of the Go server. The one difference: a
// compiled Webhook has no created_at (the store sets it).

import type {
  CompiledDefinition as StoredDefinition,
  CloudProviders,
  ValidationError,
  Webhook,
} from "@openfactory/core";

export type {
  AgentConfig,
  AgentOverrides,
  AgentType,
  Automation,
  BenchmarkAgentPin,
  BenchmarkConfiguration,
  BenchmarkSuite,
  BenchmarkTask,
  CloudProviders,
  EffectiveNetworkPolicy,
  HarnessAuth,
  HarnessConfig,
  HarnessType,
  IntegrationDecl,
  MachineSpec,
  MCPServer,
  NetworkPolicy,
  RepoAtCommit,
  RepoRef,
  RepoSubstitution,
  Schedule,
  Scorer,
  ScorerAgent,
  ScorerLabel,
  SelfImprovementConfig,
  Skill,
  SpendLimit,
  Trigger,
  ValidationError,
} from "@openfactory/core";

/** A webhook as the compiler returns it: the store adds created_at. */
export type CompiledWebhook = Omit<Webhook, "created_at">;

export type GCPProvider = NonNullable<CloudProviders["gcp"]>;

/** The compiled definition: the stored shape with compiled webhooks. */
export interface CompiledDefinition extends Omit<StoredDefinition, "webhooks"> {
  webhooks: CompiledWebhook[] | null;
}

export interface CompileResult {
  /** The compiled definition; null when there are errors. */
  definition: CompiledDefinition | null;
  /** Validation errors sorted by file, then line. */
  errors: ValidationError[];
}
