// Go JSON encoding rules for the compiled definition. Each encoder returns a
// new object with the fields in Go struct order: a field with `omitempty` is
// left out when it holds its zero value (or is an empty list or map), a
// pointer field is left out only when it is unset, and a list or map without
// `omitempty` becomes null when unset. Map keys are sorted like Go does.

import type {
  AgentConfig,
  AgentOverrides,
  Automation,
  BenchmarkAgentPin,
  BenchmarkConfiguration,
  BenchmarkSuite,
  BenchmarkTask,
  CloudProviders,
  CompiledDefinition,
  EffectiveNetworkPolicy,
  GCPProvider,
  HarnessAuth,
  HarnessConfig,
  IntegrationDecl,
  MachineSpec,
  MCPServer,
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
  CompiledWebhook,
} from "./types.ts";
import { put } from "./yamlnode.ts";

function isEmpty(v: unknown): boolean {
  if (v === undefined || v === null || v === "" || v === 0 || v === false) return true;
  if (Array.isArray(v)) return v.length === 0;
  if (typeof v === "object") return Object.keys(v as object).length === 0;
  return false;
}

/** The Go zero values: req writes one when the value is unset, even for a Go string that TypeScript types as a union. */
type GoZero = "" | 0 | false | null;

/** The keys of T that are not optional. */
type RequiredKeys<T> = { [K in keyof T]-?: object extends Pick<T, K> ? never : K }[keyof T];

/**
 * Out builds the JSON object of a T field by field, in Go struct order. S
 * collects the keys written so far; done() compiles only when every required
 * key of T is written.
 */
class Out<T, S extends keyof T = never> {
  private readonly o: Record<PropertyKey, unknown> = {};
  /** req writes a field without omitempty: v, or zero when v is unset. */
  req<K extends keyof T>(k: K, v: T[K] | undefined, zero: T[K] | GoZero): Out<T, S | K> {
    this.o[k] = v === undefined ? zero : v;
    return this as Out<T, S | K>;
  }
  /** omit writes an omitempty field unless it holds its zero value or is empty. */
  omit<K extends keyof T>(k: K, v: T[K] | null | undefined): Out<T, S | K> {
    if (!isEmpty(v)) this.o[k] = v;
    return this as Out<T, S | K>;
  }
  /** ptr writes a pointer field unless it is unset. */
  ptr<K extends keyof T>(k: K, v: T[K] | null | undefined): Out<T, S | K> {
    if (v !== undefined && v !== null) this.o[k] = v;
    return this as Out<T, S | K>;
  }
  done(...missing: [Exclude<RequiredKeys<T>, S>] extends [never] ? [] : [missing: Exclude<RequiredKeys<T>, S>]): T {
    void missing;
    return this.o as unknown as T;
  }
}

function list<T, R>(v: T[] | null | undefined, f: (x: T) => R): R[] | null {
  return v === null || v === undefined ? null : v.map(f);
}

/** sortedMap returns m with sorted keys (values mapped by f); null stays null. */
function sortedMap<T, R>(m: Record<string, T> | null | undefined, f: (x: T) => R): Record<string, R> | null {
  if (m === null || m === undefined) return null;
  const out: Record<string, R> = {};
  for (const k of Object.keys(m).sort()) put(out, k, f(m[k] as T));
  return out;
}

const id = <T>(x: T): T => x;

export function encodeAuth(a: HarnessAuth): HarnessAuth {
  return new Out<HarnessAuth>().req("source", a.source, "").omit("secretName", a.secretName).done();
}

export function encodeHarness(h: HarnessConfig): HarnessConfig {
  return new Out<HarnessConfig>()
    .req("type", h.type, "")
    .req("model", h.model, "")
    .omit("reasoningLevel", h.reasoningLevel)
    .ptr("auth", h.auth ? encodeAuth(h.auth) : undefined)
    .done();
}

export function encodeMCPServer(s: MCPServer): MCPServer {
  return new Out<MCPServer>()
    .omit("managedId", s.managedId)
    .omit("command", s.command)
    .omit("args", s.args)
    .omit("env", sortedMap(s.env, id))
    .omit("url", s.url)
    .omit("headers", sortedMap(s.headers, id))
    .omit("allowWrite", s.allowWrite)
    .done();
}

function mcpMap(m: Record<string, MCPServer> | null | undefined): Record<string, MCPServer> | null {
  return sortedMap(m, encodeMCPServer);
}

function encodeNetwork(n: EffectiveNetworkPolicy): EffectiveNetworkPolicy {
  return new Out<EffectiveNetworkPolicy>()
    .req("restricted", n.restricted, false)
    .req("allowed_hosts", n.allowed_hosts, null)
    .req("denied_hosts", n.denied_hosts, null)
    .done();
}

export function encodeSkill(s: Skill): Skill {
  return new Out<Skill>()
    .req("name", s.name, "")
    .req("description", s.description, "")
    .req("path", s.path, "")
    .req("content", s.content, "")
    .omit("files", sortedMap(s.files, id))
    .req("scope", s.scope, "")
    .done();
}

export function encodeMachine(m: MachineSpec): MachineSpec {
  return new Out<MachineSpec>()
    .req("name", m.name, "")
    .omit("description", m.description)
    .omit("setupCommands", m.setupCommands)
    .omit("vcpus", m.vcpus)
    .omit("memoryGb", m.memoryGb)
    .req("os", m.os, "")
    .req("arch", m.arch, "")
    .omit("image", m.image)
    .omit("macVersion", m.macVersion)
    .omit("registryCredentialSecretName", m.registryCredentialSecretName)
    .omit("user", m.user)
    .omit("failureSessionRetentionMinutes", m.failureSessionRetentionMinutes)
    .done();
}

export function encodeAgent(a: AgentConfig): AgentConfig {
  return new Out<AgentConfig>()
    .req("id", a.id, "")
    .req("factory_id", a.factory_id, "")
    .req("name", a.name, "")
    .req("description", a.description, "")
    .req("type", a.type, "")
    .req("instructions", a.instructions, "")
    .req("harness", encodeHarness(a.harness ?? { type: "", model: "" }), null)
    .ptr("machine", a.machine ? encodeMachine(a.machine) : undefined)
    .omit("environment_id", a.environment_id)
    .omit("worker_host", a.worker_host)
    .omit("secrets", a.secrets)
    .omit("mcp_servers", mcpMap(a.mcp_servers))
    .req(
      "network_policy",
      encodeNetwork(a.network_policy ?? { restricted: false, allowed_hosts: null, denied_hosts: null }),
      null,
    )
    .req("spawnable_by", a.spawnable_by, null)
    .omit("idle_timeout_minutes", a.idle_timeout_minutes)
    .omit("idle_timeout_set", a.idle_timeout_set)
    .omit("credential_strategy", a.credential_strategy)
    .omit("skills", list(a.skills, encodeSkill))
    .ptr("snapshot", a.snapshot)
    .done();
}

function encodeSchedule(s: Schedule): Schedule {
  return new Out<Schedule>().omit("name", s.name).req("cron", s.cron, "").done();
}

export function encodeTrigger(t: Trigger): Trigger {
  return new Out<Trigger>()
    .req("provider", t.provider, "")
    .req("event", t.event, "")
    .ptr("filter", t.filter)
    .ptr("schedule", t.schedule ? encodeSchedule(t.schedule) : undefined)
    .omit("concurrency", t.concurrency)
    .done();
}

export function encodeOverrides(o: AgentOverrides): AgentOverrides {
  return new Out<AgentOverrides>()
    .ptr("harness", o.harness ? encodeHarness(o.harness) : undefined)
    .omit("model", o.model)
    .omit("machine", o.machine)
    .ptr("environment_id", o.environment_id)
    .req("secrets", o.secrets, null)
    .req("mcp_servers", mcpMap(o.mcp_servers), null)
    .ptr("worker_host", o.worker_host)
    .ptr("idle_timeout_minutes", o.idle_timeout_minutes)
    .done();
}

export function encodeAutomation(a: Automation): Automation {
  return new Out<Automation>()
    .req("id", a.id, "")
    .req("factory_id", a.factory_id, "")
    .req("name", a.name, "")
    .omit("display_name", a.display_name)
    .req("enabled", a.enabled, false)
    .req("agent", a.agent, "")
    .req("prompt", a.prompt, "")
    .req("triggers", list(a.triggers, encodeTrigger), null)
    .omit("run_as", a.run_as)
    .ptr("overrides", a.overrides ? encodeOverrides(a.overrides) : undefined)
    .done();
}

export function encodeWebhook(w: CompiledWebhook): CompiledWebhook {
  return new Out<CompiledWebhook>()
    .req("id", w.id, "")
    .req("factory_id", w.factory_id, "")
    .req("name", w.name, "")
    .req("auth_mode", w.auth_mode, "")
    .omit("signature_scheme", w.signature_scheme)
    .req("secret_name", w.secret_name, "")
    .omit("delivery_id_header", w.delivery_id_header)
    .req("enabled", w.enabled, false)
    .done();
}

function encodeLabel(l: ScorerLabel): ScorerLabel {
  return new Out<ScorerLabel>()
    .req("value", l.value, "")
    .omit("description", l.description)
    .req("score", l.score, 0)
    .done();
}

function encodeScorerAgent(a: ScorerAgent): ScorerAgent {
  return new Out<ScorerAgent>().req("name", a.name, "").omit("includeDescendants", a.includeDescendants).done();
}

export function encodeScorer(s: Scorer): Scorer {
  return new Out<Scorer>()
    .req("id", s.id, "")
    .req("factory_id", s.factory_id, "")
    .req("slug", s.slug, "")
    .req("name", s.name, "")
    .omit("description", s.description)
    .req("agents", list(s.agents, encodeScorerAgent), null)
    .req("labels", list(s.labels, encodeLabel), null)
    .req("passing_score", s.passing_score, 0)
    .req("sampling_rate", s.sampling_rate, 0)
    .req("model", s.model, "")
    .req("rubric", s.rubric, "")
    .req("self_improvement", s.self_improvement, false)
    .omit("machine", s.machine)
    .omit("secrets", s.secrets)
    .omit("mcp_servers", mcpMap(s.mcp_servers))
    .ptr("harness", s.harness ? encodeHarness(s.harness) : undefined)
    .done();
}

function encodeRepo(r: RepoRef): RepoRef {
  return new Out<RepoRef>().req("codeForge", r.codeForge, "").req("owner", r.owner, "").req("name", r.name, "").done();
}

function encodePin(p: BenchmarkAgentPin): BenchmarkAgentPin {
  return new Out<BenchmarkAgentPin>()
    .req("agent", p.agent, "")
    .omit("model", p.model)
    .ptr("harness", p.harness ? encodeHarness(p.harness) : undefined)
    .done();
}

function encodeConfig(c: BenchmarkConfiguration): BenchmarkConfiguration {
  return new Out<BenchmarkConfiguration>()
    .req("name", c.name, "")
    .omit("agents", list(c.agents, encodePin))
    .omit("machine", c.machine)
    .done();
}

function encodeRepoAt(r: RepoAtCommit): RepoAtCommit {
  return new Out<RepoAtCommit>()
    .req("codeForge", r.codeForge, "")
    .req("owner", r.owner, "")
    .req("repo", r.repo, "")
    .req("ref", r.ref, "")
    .done();
}

function encodeTask(t: BenchmarkTask): BenchmarkTask {
  return new Out<BenchmarkTask>()
    .req("slug", t.slug, "")
    .req("title", t.title, "")
    .req("prompt", t.prompt, "")
    .req("successCriteria", t.successCriteria, "")
    .omit("sourceRunId", t.sourceRunId)
    .omit("tags", t.tags)
    .ptr("trackerSeed", t.trackerSeed)
    .omit("startingRepoRefs", list(t.startingRepoRefs, encodeRepoAt))
    .done();
}

export function encodeSuite(s: BenchmarkSuite): BenchmarkSuite {
  return new Out<BenchmarkSuite>()
    .req("slug", s.slug, "")
    .req("name", s.name, "")
    .omit("description", s.description)
    .req("agent", s.agent, "")
    .omit("configurations", list(s.configurations, encodeConfig))
    .omit("tasks", list(s.tasks, encodeTask))
    .done();
}

function encodeIntegration(i: IntegrationDecl): IntegrationDecl {
  return new Out<IntegrationDecl>().req("type", i.type, "").omit("settings", sortedMap(i.settings, id)).done();
}

function encodeSelfImprovement(s: SelfImprovementConfig): SelfImprovementConfig {
  return new Out<SelfImprovementConfig>()
    .omit("failedRunThreshold", s.failedRunThreshold)
    .omit("reviewerType", s.reviewerType)
    .omit("reviewerEmails", s.reviewerEmails)
    .omit("analysisModel", s.analysisModel)
    .done();
}

function encodeGCP(g: GCPProvider): GCPProvider {
  return new Out<GCPProvider>()
    .req("projectNumber", g.projectNumber, "")
    .req("workloadIdentityFederationPoolId", g.workloadIdentityFederationPoolId, "")
    .req("workloadIdentityFederationProviderId", g.workloadIdentityFederationProviderId, "")
    .omit("serviceAccountEmail", g.serviceAccountEmail)
    .done();
}

function encodeCloud(c: CloudProviders): CloudProviders {
  return new Out<CloudProviders>()
    .ptr("gcp", c.gcp ? encodeGCP(c.gcp) : undefined)
    .omit("audiences", c.audiences)
    .done();
}

function encodeSpend(s: SpendLimit): SpendLimit {
  return new Out<SpendLimit>().omit("daily", s.daily).omit("monthly", s.monthly).done();
}

function encodeSub(s: RepoSubstitution): RepoSubstitution {
  return { source: encodeRepo(s.source), target: encodeRepo(s.target) };
}

/** encodeDefinition returns def in the JSON shape of the Go CompiledDefinition. */
export function encodeDefinition(d: CompiledDefinition): CompiledDefinition {
  return new Out<CompiledDefinition>()
    .req("schemaVersion", d.schemaVersion, "")
    .req("name", d.name, "")
    .omit("description", d.description)
    .omit("alias", d.alias)
    .omit("credentialStrategy", d.credentialStrategy)
    .omit("codeForges", d.codeForges)
    .req("repositories", list(d.repositories, encodeRepo), null)
    .omit("integrations", list(d.integrations, encodeIntegration))
    .req("integrationsSet", d.integrationsSet, false)
    .omit("secrets", d.secrets)
    .omit("mcpServers", mcpMap(d.mcpServers))
    .ptr("cloudProviders", d.cloudProviders ? encodeCloud(d.cloudProviders) : undefined)
    .ptr("selfImprovement", d.selfImprovement ? encodeSelfImprovement(d.selfImprovement) : undefined)
    .req("githubOnboardingNotices", d.githubOnboardingNotices, false)
    .omit("factoryConfigChecks", d.factoryConfigChecks)
    .req("agents", list(d.agents, encodeAgent), null)
    .req("automations", list(d.automations, encodeAutomation), null)
    .req("machines", sortedMap(d.machines, encodeMachine), null)
    .req("scorers", list(d.scorers, encodeScorer), null)
    .ptr("scorerDefaults", d.scorerDefaults ? encodeOverrides(d.scorerDefaults) : undefined)
    .req("webhooks", list(d.webhooks, encodeWebhook), null)
    .req("benchmarks", list(d.benchmarks, encodeSuite), null)
    .omit("benchmarkRepoSubstitutions", list(d.benchmarkRepoSubstitutions, encodeSub))
    .req("skills", list(d.skills, encodeSkill), null)
    .omit("maxConcurrentRuns", d.maxConcurrentRuns)
    .omit("allowAgentMerge", d.allowAgentMerge)
    .ptr("spendLimit", d.spendLimit ? encodeSpend(d.spendLimit) : undefined)
    .omit("approvers", d.approvers)
    .omit("restrictFollowupsToStarter", d.restrictFollowupsToStarter)
    .omit("requireLinkedActor", d.requireLinkedActor)
    .done();
}
