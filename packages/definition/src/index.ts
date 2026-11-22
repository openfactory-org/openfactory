// @openfactory/definition: parse, validate, compile, resolve and diff factory
// definition trees. Port of internal/definition. Worker-safe (no Node APIs);
// loadDir is in "@openfactory/definition/node".

export type * from './types.ts';
export { compile, normalizePath } from './compile.ts';
export {
  BUILTIN_MCP_PREFIX,
  BUILTIN_MCP_PROVIDERS,
  builtinMCPProvider,
  RESERVED_MCP_SERVER_NAME,
  SCHEMA_VERSION,
} from './common.ts';
export { encodeDefinition } from './encode.ts';
export {
  type Change,
  type ChangeAction,
  CHANGE_ADDED,
  CHANGE_CHANGED,
  CHANGE_REMOVED,
  canonicalJSON,
  diff,
  diffCounts,
  diffEmpty,
  diffSummary,
  type DiffResult,
  KIND_AGENT,
  KIND_AUTOMATION,
  KIND_BENCHMARK,
  KIND_FACTORY,
  KIND_MACHINE,
  KIND_SCORER,
  KIND_SKILL,
  KIND_WEBHOOK,
} from './diff.ts';
export { DEFAULT_AGENT_NAMES, DEFAULT_MODEL, defaultAgents, defaultDefinition, type DefaultDefinitionOptions } from './defaults.ts';
export { keyLine } from './locate.ts';
export { validateCron } from './cron.ts';
export { parseRepoAtCommit, roundHundredths } from './extras.ts';
export { MAX_WEBHOOKS, SIGNATURE_SCHEMES, validDeliveryIdHeader, validURLTokenSecret } from './resources.ts';
export { parseRunAs, RUN_AS_FACTORY } from './automations.ts';
export {
  type EventSpec,
  normalizeProvider,
  triggerEvents,
  triggerProviders,
  WEBHOOK_UID_PREFIX,
  SCHEDULE_CONCURRENCY_ALLOW,
  SCHEDULE_CONCURRENCY_SKIP,
} from './triggers.ts';
export { SCHEMA_BASE_URL, SCHEMA_NAMES, schema, schemaForPath, schemas, schemaVersions, type SchemaCatalog } from './schema.ts';
export { splitFrontmatter } from './yamlnode.ts';
export * as harnessModels from './harness-models.ts';
export * as networkPolicy from './networkpolicy.ts';
