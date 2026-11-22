// Self-improvement: failures, trigger rule, reviewers and the prompt. Port of
// the pure parts of improve.go. The orchestration is in service.ts.
import type { CompiledDefinition, Factory, Run, Score, Scorer } from "@openfactory/core";
import { scorerName } from "./judge";
import { f2, oneLine, truncate } from "./text";
import { clampThreshold, DEFAULT_FAILED_RUN_THRESHOLD, type RunsPort } from "./types";

export const REVIEWER_ADMINS = "admins";
export const REVIEWER_TEAM = "team";
export const REVIEWER_CUSTOM = "custom";
export const REVIEWER_NONE = "none";

const MAX_FAILURES_IN_PROMPT = 25;
const MAX_FAILURE_EXCERPT = 3000;
const MAX_PROMPT_FILE_BYTES = 20_000;
const MAX_PROMPT_FILES_TOTAL = 120_000;
const MAX_PROMPT_EXCERPT = 1500;

/** One unreviewed failed score feeding Self-improvement. */
export interface Failure {
  score: Score;
  scorer: Scorer;
}

export interface ImprovementSettings {
  threshold: number;
  reviewerType: string;
  reviewerEmails: string[];
  analysisModel: string;
}

/** The effective selfImprovement configuration. */
export function improvementSettings(
  def: Pick<CompiledDefinition, "selfImprovement"> | null | undefined,
  defaults: { threshold?: number; analysisModel?: string },
): ImprovementSettings {
  const cfg: ImprovementSettings = {
    threshold: clampThreshold(defaults.threshold && defaults.threshold > 0 ? defaults.threshold : DEFAULT_FAILED_RUN_THRESHOLD),
    reviewerType: REVIEWER_ADMINS,
    reviewerEmails: [],
    analysisModel: "",
  };
  const si = def?.selfImprovement;
  if (si) {
    if (si.failedRunThreshold && si.failedRunThreshold > 0) cfg.threshold = clampThreshold(si.failedRunThreshold);
    if (si.reviewerType) cfg.reviewerType = si.reviewerType;
    cfg.reviewerEmails = si.reviewerEmails ?? [];
    cfg.analysisModel = si.analysisModel ?? "";
  }
  if (cfg.analysisModel === "") cfg.analysisModel = defaults.analysisModel ?? "";
  return cfg;
}

const ms = (t: string): number => Date.parse(t);

/**
 * Groups failing scores per agent: one failure per run (the latest), oldest
 * first. Scores that are passed, attached to a run already, or have no agent
 * are skipped.
 */
export function groupFailures(list: readonly Failure[]): Map<string, Failure[]> {
  const byRun = new Map<string, Map<string, Failure>>();
  for (const f of list) {
    const s = f.score;
    if (s.passed || s.improvement_run_id || !s.agent_name || s.scorer_id !== f.scorer.id || !f.scorer.self_improvement) continue;
    let m = byRun.get(s.agent_name);
    if (!m) byRun.set(s.agent_name, (m = new Map()));
    const prev = m.get(s.run_id);
    if (!prev || ms(s.created_at) > ms(prev.score.created_at)) m.set(s.run_id, f);
  }
  const out = new Map<string, Failure[]>();
  for (const [agent, m] of byRun) {
    const items = [...m.values()].sort((a, b) => {
      const d = ms(a.score.created_at) - ms(b.score.created_at);
      if (d !== 0) return d;
      return a.score.id < b.score.id ? -1 : a.score.id > b.score.id ? 1 : 0;
    });
    out.set(agent, items);
  }
  return out;
}

/** The trigger rule: enough failures, or the oldest one is old enough (maxFailureAgeMs <= 0 disables the age rule). */
export function thresholdReached(failures: readonly Failure[], threshold: number, nowMs: number, maxFailureAgeMs: number): boolean {
  if (failures.length >= threshold) return true;
  if (maxFailureAgeMs > 0 && failures.length > 0) return nowMs - ms(failures[0]!.score.created_at) >= maxFailureAgeMs;
  return false;
}

export interface UserLite {
  id: string;
  email: string;
  role: string;
}

/**
 * Team members by reviewer type: admins (team admins), team (any member),
 * custom (members whose email is listed), none (nobody). An unknown type is
 * admins. Ordered by user ID.
 */
export function eligibleReviewers(users: readonly UserLite[], reviewerType: string, emails: readonly string[]): UserLite[] {
  let pool: UserLite[];
  switch (reviewerType) {
    case REVIEWER_NONE:
      return [];
    case REVIEWER_TEAM:
      pool = [...users];
      break;
    case REVIEWER_CUSTOM: {
      const want = new Set(emails.map((e) => e.trim().toLowerCase()));
      pool = users.filter((u) => want.has(u.email.toLowerCase()));
      break;
    }
    default:
      pool = users.filter((u) => u.role === "admin");
  }
  return pool.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}

/** The "Regressions addressed" section. */
export function regressionsSection(failures: readonly Failure[], runUrl: (runId: string) => string): string {
  let b = "## Regressions addressed\n\n";
  for (const fl of failures) {
    let ref = fl.score.run_id;
    const u = runUrl(fl.score.run_id);
    if (u) ref = `[${fl.score.run_id}](${u})`;
    b += `- Run ${ref}: scorer \`${scorerName(fl.scorer)}\` labeled \`${fl.score.label}\` (score ${f2(fl.score.score)}, passing ${f2(fl.scorer.passing_score)})\n`;
  }
  return b;
}

/** The tail of a run's conversation: the last assistant messages plus its result or error. */
export async function runSummary(runs: Pick<RunsPort, "listMessages">, r: Pick<Run, "id" | "result" | "error">): Promise<string> {
  let msgs: { role: string; content: string }[];
  try {
    msgs = await runs.listMessages(r.id);
  } catch {
    return truncate((r.result ?? "") + (r.error ?? ""), MAX_FAILURE_EXCERPT);
  }
  const parts: string[] = [];
  for (let i = msgs.length - 1; i >= 0 && parts.length < 3; i--) {
    const m = msgs[i]!;
    if (m.role === "assistant" && m.content.trim() !== "") parts.push(truncate(m.content, 1000));
  }
  parts.reverse();
  if (r.error) parts.push("ERROR: " + truncate(r.error, 1000));
  else if (r.result && (parts.length === 0 || parts[parts.length - 1] !== r.result)) parts.push("RESULT: " + truncate(r.result, 1000));
  return truncate(parts.join("\n---\n"), MAX_FAILURE_EXCERPT);
}

function scorerRubrics(failures: readonly Failure[]): string {
  const seen = new Set<string>();
  let b = "";
  for (const fl of failures) {
    if (seen.has(fl.scorer.id)) continue;
    seen.add(fl.scorer.id);
    b += `### ${scorerName(fl.scorer)}\n\n${fl.scorer.rubric.trim()}\n\nLabels:\n`;
    for (const l of fl.scorer.labels ?? []) {
      b += `- \`${l.value}\` (score ${f2(l.score)})`;
      if (l.description) b += `: ${oneLine(l.description)}`;
      b += "\n";
    }
    b += "\n";
  }
  return b;
}

/** The directory prefix (with a trailing slash, or "") that holds factory.yaml in a file map. */
export function definitionRoot(files: Record<string, string> | null | undefined): string {
  let best = "";
  let found = false;
  for (const p of Object.keys(files ?? {})) {
    const base = p.slice(p.lastIndexOf("/") + 1);
    if (base !== "factory.yaml") continue;
    const dir = p.slice(0, p.length - "factory.yaml".length);
    if (!found || dir.length < best.length) {
      best = dir;
      found = true;
    }
  }
  return best;
}

const sortStr = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0);

/** The relevant definition files in full (factory.yaml, the agent's files, the failing scorers), and a list of the rest. */
export function definitionContext(files: Record<string, string>, agent: string, failures: readonly Failure[]): string {
  const root = definitionRoot(files);
  const rel = (p: string): string => (p.startsWith(root) ? p.slice(root.length) : p);
  const scorerDirs = new Set<string>();
  for (const fl of failures) if (fl.scorer.slug) scorerDirs.add(`scorers/${fl.scorer.slug}/`);
  const relevant = (p: string): boolean => {
    const r = rel(p);
    if (r === "factory.yaml" || r.startsWith(`agents/${agent}/`)) return true;
    for (const d of scorerDirs) if (r.startsWith(d)) return true;
    return false;
  };
  const paths = Object.keys(files).sort(sortStr);
  let b = "All files:\n";
  for (const p of paths) b += `- \`${p}\`\n`;
  b += "\n";
  let total = 0;
  for (const p of paths) {
    if (!relevant(p)) continue;
    const content = truncate(files[p]!, MAX_PROMPT_FILE_BYTES);
    const size = new TextEncoder().encode(content).length;
    if (total + size > MAX_PROMPT_FILES_TOTAL) {
      b += "(Further files omitted to fit the prompt; read them from the definition source.)\n";
      break;
    }
    total += size;
    const dot = p.lastIndexOf(".");
    const slash = p.lastIndexOf("/");
    let lang = dot > slash ? p.slice(dot + 1) : "";
    if (lang === "yml") lang = "yaml";
    b += `#### \`${p}\`\n\n\`\`\`${lang}\n${content}\n\`\`\`\n\n`;
  }
  return b;
}

export interface PromptInput {
  factory: Pick<Factory, "name" | "source" | "git_repo" | "git_branch" | "git_path">;
  files?: Record<string, string>;
  agent: string;
  failures: readonly Failure[];
  runs: ReadonlyMap<string, Run>;
  runUrl: (runId: string) => string;
  runsPort: Pick<RunsPort, "listMessages">;
}

/** The starting prompt of a Self-improvement run. */
export async function improvementPrompt(p: PromptInput): Promise<string> {
  const { factory: f, agent, failures } = p;
  let b = `You are running Self-improvement for the factory ${JSON.stringify(f.name)}.\n\n`;
  b += `Scorers flagged ${failures.length} distinct failing runs of the agent \`${agent}\`. `;
  b += "Investigate why these runs failed, find the common root cause, and propose a fix as a pull request.\n\n";

  b += "## What to change\n\n";
  b += "Pick the smallest change that addresses the root cause:\n";
  b += `- The factory definition: the agent's instructions (\`agents/${agent}/agent.md\`), its skills (\`agents/${agent}/skills/*/SKILL.md\` or factory-wide \`skills/*/SKILL.md\`), its model/harness, or other configuration in \`factory.yaml\`.\n`;
  b += "- Application code, when the failures are caused by the repository itself (missing scripts, broken tooling, unclear docs).\n\n";
  b += "Do not weaken or edit the scorers to make failures disappear. Keep definition changes valid against the definition schema.\n\n";

  b += "## How to deliver\n\n";
  if (f.source === "git" && f.git_repo) {
    const branch = f.git_branch || "main";
    b += `The factory definition lives in \`${f.git_repo}\` (production branch \`${branch}\``;
    if (f.git_path) b += `, directory \`${f.git_path}\``;
    b += "). Open a pull request against that branch for definition changes; definition checks validate it and merging applies it.\n";
  } else {
    b +=
      "The factory definition is managed by Open Factory (not in a git repository). For definition changes, call the factory tool `propose_definition_change` with the full new content of every changed file, keyed by path, and a short message. The tool checks the files and stores a proposed version; a reviewer sees the diff in the inbox and applies it with one approval. If the tool reports errors, fix the files and call it again. A rejection comes back to you as a message with the reason.\n";
  }
  b += "Open application-code changes as a pull request against the affected repository.\n";
  b +=
    "Every pull request body must include a `Regressions addressed` section listing the failing runs and scorer results below, so reviewers can trace the change to its evidence. Nothing is adopted without human review.\n\n";

  b += regressionsSection(failures, p.runUrl);
  b += "\n## Failing runs\n\n";
  let shown = failures;
  if (shown.length > MAX_FAILURES_IN_PROMPT) {
    shown = shown.slice(shown.length - MAX_FAILURES_IN_PROMPT);
    b += `(Showing the ${MAX_FAILURES_IN_PROMPT} most recent of ${failures.length} failures.)\n\n`;
  }
  for (let i = 0; i < shown.length; i++) {
    const fl = shown[i]!;
    b += `### ${i + 1}. Run ${fl.score.run_id}\n\n`;
    b += `- Scorer: \`${scorerName(fl.scorer)}\``;
    if (fl.scorer.description) b += ` — ${oneLine(fl.scorer.description)}`;
    b += "\n";
    b += `- Classification: \`${fl.score.label}\` (score ${f2(fl.score.score)}; passing score ${f2(fl.scorer.passing_score)})\n`;
    const u = p.runUrl(fl.score.run_id);
    if (u) b += `- Link: ${u}\n`;
    if (fl.score.rationale) b += `- Judge rationale: ${truncate(oneLine(fl.score.rationale), 1500)}\n`;
    const r = p.runs.get(fl.score.run_id);
    if (r) {
      b += `- Run state: ${r.state}\n`;
      if (r.prompt) b += `\nPrompt excerpt:\n\n\`\`\`\n${truncate(r.prompt, MAX_PROMPT_EXCERPT)}\n\`\`\`\n`;
      const summary = await runSummary(p.runsPort, r);
      if (summary) b += `\nTranscript summary (final messages):\n\n\`\`\`\n${summary}\n\`\`\`\n`;
    }
    b += "\n";
  }
  const rubrics = scorerRubrics(failures);
  if (rubrics) b += "## Scorer criteria\n\n" + rubrics;
  if (p.files && Object.keys(p.files).length > 0) {
    b += "## Current factory definition\n\n" + definitionContext(p.files, agent, failures);
  }
  return b;
}
