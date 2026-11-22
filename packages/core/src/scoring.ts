// Scorers, scores and benchmark runs. Port of internal/core (scoring.go,
// types.go). Field names are the Go JSON names.

import type { Timestamp } from "./common";
import type { HarnessConfig, MCPServer } from "./definition";
import type { Usage } from "./run";

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

export const BENCHMARK_RUN_STATES = ["running", "scoring", "complete", "failed", "cancelled"] as const;
export type BenchmarkRunState = (typeof BENCHMARK_RUN_STATES)[number];

export interface BenchmarkRun {
  id: string;
  factory_id: string;
  suite: string;
  state: BenchmarkRunState;
  results?: unknown;
  created_at: Timestamp;
  finished_at?: Timestamp;
}
