// Types, metadata keys and ports of the scoring service (internal/scoring).
import type {
  BenchmarkAgentPin,
  BenchmarkConfiguration,
  BenchmarkSuite,
  CompiledDefinition,
  HarnessConfig,
  MCPServer,
  RepoAtCommit,
  RepoStart,
  RunMessage,
  RunState,
  Run,
  Score,
  Scorer,
  SourceRef,
  WorkItem,
  ValidationError,
} from "@openfactory/core";

// ---------- Metadata keys (become OF_METADATA_* in the sandbox) ----------

export const META_JUDGE = "judge";
export const META_JUDGE_SCORER = "judge_scorer";
export const META_JUDGE_SCORED_RUN = "judge_scored_run";
export const META_JUDGE_BENCHMARK = "judge_benchmark";
export const META_JUDGE_TRIAL = "judge_trial";
export const META_JUDGE_KEY = "judge_key";
export const JUDGE_SCORE = "score";
export const JUDGE_TRIAL = "trial";
export const TRIGGERED_BY_SCORER = "scorer:";

export const META_SELF_IMPROVEMENT = "self_improvement";
export const META_SELF_IMPROVEMENT_AGENT = "self_improvement_agent";
export const TRIGGERED_BY_SELF_IMPROVEMENT = "self_improvement";
export const SOURCE_SELF_IMPROVEMENT = "self_improvement";

export const META_BENCHMARK_RUN = "benchmark_run";
export const META_BENCHMARK_TRIAL = "benchmark_trial";
export const META_BENCHMARK_CONFIGURATION = "benchmark_configuration";
export const META_BENCHMARK_TASK = "benchmark_task";
export const META_BENCHMARK_REPETITION = "benchmark_repetition";
export const META_TRIAL_MODE = "trial_mode";

// ---------- Defaults ----------

export const DEFAULT_FAILED_RUN_THRESHOLD = 25;
export const MIN_FAILED_RUN_THRESHOLD = 1;
export const MAX_FAILED_RUN_THRESHOLD = 50;
export const DEFAULT_MAX_FAILURE_AGE_MS = 7 * 24 * 3600 * 1000;
export const DEFAULT_CHECK_INTERVAL_MS = 15 * 60 * 1000;
export const DEFAULT_MAX_TRANSCRIPT_BYTES = 200_000;
export const DEFAULT_MAX_JUDGE_FILE_BYTES = 8 << 20;
export const DEFAULT_CORRECTNESS_MODEL = "claude-sonnet-4-5";
export const MAX_ANCESTOR_DEPTH = 32;
export const MAX_DESCENDANTS = 200;
export const MAX_METADATA_VALUE = 256;

export const MAX_BENCHMARK_CONFIGURATIONS = 6;
export const MAX_BENCHMARK_REPETITIONS = 10;

export const clampThreshold = (n: number): number =>
  n < MIN_FAILED_RUN_THRESHOLD ? MIN_FAILED_RUN_THRESHOLD : n > MAX_FAILED_RUN_THRESHOLD ? MAX_FAILED_RUN_THRESHOLD : n;

/** A run state the scoring service acts on: terminal, or waiting for input. */
export const finishedForScoring = (s: RunState): boolean =>
  s === "succeeded" || s === "failed" || s === "cancelled" || s === "error" || s === "idle" || s === "blocked";

export const isTerminalState = (s: RunState): boolean => s === "succeeded" || s === "failed" || s === "cancelled" || s === "error";
export const waitsForInput = (s: RunState): boolean => s === "idle" || s === "blocked";

// ---------- Requests to the engine ----------

/** Sparse execution overrides (core.AgentOverrides). */
export interface Overrides {
  harness?: HarnessConfig;
  model?: string;
  machine?: string;
  environment_id?: string | null;
  secrets?: string[] | null;
  mcp_servers?: Record<string, MCPServer> | null;
  worker_host?: string | null;
  idle_timeout_minutes?: number | null;
}

/** core.DispatchRequest. */
export interface DispatchRequest {
  factoryId: string;
  prompt: string;
  source: SourceRef;
  agentName?: string;
  creatorId?: string;
  title?: string;
  overrides?: Overrides | null;
  metadata?: Record<string, string>;
  triggeredBy?: string;
  startingRepoRefs?: RepoAtCommit[];
  agentPins?: Record<string, Overrides>;
}

/** core.JudgeRunRequest. */
export interface JudgeRunRequest {
  factoryId: string;
  name: string;
  instructions: string;
  prompt: string;
  harness: HarnessConfig;
  machine?: string;
  secrets?: string[];
  mcpServers?: Record<string, MCPServer>;
  files: Record<string, string>;
  startingRepoRefs: RepoAtCommit[];
  metadata?: Record<string, string>;
  triggeredBy?: string;
}

/** The execution settings of a judge run. */
export interface JudgeExec {
  harness?: HarnessConfig;
  machine?: string;
  secrets?: string[];
  mcp_servers?: Record<string, MCPServer>;
}

// ---------- Ports (dependencies of the service, injected by the wiring) ----------

/** The engine of the WorkDO side: dispatch, judge runs, cancel, inbox. */
export interface EnginePort {
  /** Dispatches a root run. The caller may fix the work item's partition for idempotency. */
  dispatch(req: DispatchRequest, idempotencyKey?: string): Promise<{ workItemId?: string; runId: string }>;
  /**
   * Starts a judge run. `runId` is chosen by the caller, so a retry of the same
   * call reaches the same run (the WorkDO returns the run that exists).
   */
  startJudge(req: JudgeRunRequest, runId: string): Promise<{ runId: string }>;
  cancel(runId: string, actor: string): Promise<void>;
  followUp(runId: string, message: string, actor: string): Promise<void>;
  /** Throws conflict when the item is resolved, not_found when it does not exist. */
  resolveInbox(inboxId: string, resolution: string, actor: string): Promise<void>;
  applyDefinition(factoryId: string, files: Record<string, string>, actor: string, message: string): Promise<{ id: string }>;
  checkDefinition(factoryId: string, files: Record<string, string>): Promise<{ valid: boolean; errors: ValidationError[] }>;
  /**
   * Creates an inbox item. An item of a work item is owned by the WorkDO of that
   * work item; an item without one is a team-level item owned by IndexDO.
   * The item ID is chosen by the caller (idempotent).
   */
  createInboxItem(item: InboxItemInput): Promise<void>;
}

export interface InboxItemInput {
  id: string;
  factory_id: string;
  work_item_id?: string;
  run_id?: string;
  kind: string;
  title: string;
  body: string;
  options?: string[];
  assignee_id?: string;
  created_at: string;
}

/** Read access to runs and their messages (WorkDO and IndexDO). */
export interface RunsPort {
  getRun(runId: string): Promise<Run | null>;
  getWorkItem(workItemId: string): Promise<WorkItem | null>;
  /** Direct children of a run, any order. */
  listChildren(runId: string): Promise<Run[]>;
  /** Messages of a run in sequence order (compacted messages included). */
  listMessages(runId: string): Promise<Pick<RunMessage, "role" | "content">[]>;
  /** The start commits recorded for a run (runner path). */
  loadStartCommits(runId: string): Promise<RepoStart[]>;
  /** Open inbox items of a factory (for trials that wait for a human). */
  listOpenInbox(factoryId: string): Promise<{ id: string; run_id?: string; work_item_id?: string }[]>;
}

/** What the scoring service reads from a factory. */
export interface FactoryInfo {
  id: string;
  team_id: string;
  name: string;
  source?: string;
  git_repo?: string;
  git_branch?: string;
  git_path?: string;
  active_version_id: string;
}

export interface DefinitionFiles {
  versionId: string;
  files: Record<string, string>;
}

/** TeamDO side state of the service: factories, scorers, scores, markers. */
export interface ScoringStore {
  getFactory(factoryId: string): Promise<FactoryInfo>;
  /** The compiled active definition, or null when the factory has none. */
  activeDefinition(factoryId: string): Promise<{ definition: CompiledDefinition; version: DefinitionFiles } | null>;
  listScorers(factoryId: string): Promise<Scorer[]>;
  getScore(scorerId: string, runId: string): Promise<Score | null>;
  /** Creates or updates the score of (scorer, run). Returns the stored score. */
  putScore(score: Score & { factory_id: string }): Promise<Score>;
  /** Unreviewed failed scores of scorers with selfImprovement, per factory. */
  listFailedScores(factoryId: string): Promise<{ score: Score; scorer: Scorer }[]>;
  markScoresImproved(scoreIds: string[], improvementRunId: string): Promise<void>;
  kvGet(key: string): Promise<string>;
  kvSet(key: string, value: string): Promise<void>;
  /** Sets the key to value only when it is empty. Reports whether it did. */
  kvClaim(key: string, value: string): Promise<boolean>;
  listUsers(): Promise<{ id: string; email: string; role: string }[]>;
}

export interface ScoringOptions {
  publicUrl?: string;
  correctnessModel?: string;
  analysisModel?: string;
  improvementAgent?: string;
  defaultFailedRunThreshold?: number;
  /** ms. Negative disables. */
  maxFailureAgeMs?: number;
  maxTranscriptBytes?: number;
  now?: () => number;
  /** Picks an index in [0, n). Default: crypto.getRandomValues. */
  randomInt?: (n: number) => number;
  newId?: (prefix: string) => string;
  /** Asks a reviewer on the forge. Optional. */
  requestReview?: (teamId: string, pr: { forge: string; repo: string; number: number; url: string }, userId: string) => Promise<void>;
  log?: { info(msg: string, f?: Record<string, unknown>): void; warn(msg: string, f?: Record<string, unknown>): void; error(msg: string, f?: Record<string, unknown>): void };
}

export type { BenchmarkAgentPin, BenchmarkConfiguration, BenchmarkSuite };
