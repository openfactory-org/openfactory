// Judge runs: instructions, prompt and request. Port of judge.go.
import { JUDGE_SKILL_NAME, OfError, type RepoAtCommit, type Run, type Scorer, type ScorerLabel, type Usage } from "@openfactory/core";
import { buildTranscript } from "./transcript";
import { parseJudgeAnswer } from "./sample";
import { clip, f2, oneLine } from "./text";
import {
  DEFAULT_MAX_JUDGE_FILE_BYTES,
  MAX_DESCENDANTS,
  type JudgeExec,
  type JudgeRunRequest,
  type RunsPort,
} from "./types";

export const scorerName = (sc: Pick<Scorer, "slug" | "name">): string => sc.slug || sc.name;

/** The execution settings of a scorer's judge: the scorer's model wins over its harness model. */
export function scorerExec(sc: Scorer): JudgeExec {
  const ex: JudgeExec = {};
  if (sc.machine) ex.machine = sc.machine;
  if (sc.secrets) ex.secrets = sc.secrets;
  if (sc.mcp_servers) ex.mcp_servers = sc.mcp_servers;
  if (sc.harness) {
    const h = { ...sc.harness };
    if (sc.model) h.model = sc.model;
    ex.harness = h;
  }
  return ex;
}

/** The system instructions of a judge run. */
export function judgeInstructions(name: string, rubric: string, labels: readonly ScorerLabel[]): string {
  const b: string[] = [];
  b.push(`You are the judge of the scorer ${JSON.stringify(name)}. You classify one agent run with exactly one label.\n\n`);
  b.push(`The evidence is in the skill \`${JUDGE_SKILL_NAME}\` in your workspace. Its file \`transcript.md\` holds the full transcript of the run, with its child runs. `);
  b.push("The repositories in your workspace are checked out at the commits where the run started. ");
  b.push(
    "You can read files, run commands and use your tools to check the work. Do not change anything outside your workspace: do not push, comment, post messages or open pull requests.\n\n",
  );
  b.push("## Rubric\n\n");
  b.push(rubric.trim());
  b.push("\n\n## Labels\n\n");
  for (const l of labels) {
    b.push(`- \`${l.value}\` (score ${f2(l.score)})`);
    if (l.description) b.push(`: ${oneLine(l.description)}`);
    b.push("\n");
  }
  b.push("\n## Answer\n\n");
  b.push("End your final message with one JSON object on its own line, and nothing after it:\n\n");
  b.push('{"label": "<one label value from the list>", "rationale": "<why, in a few sentences>"}\n');
  return b.join("");
}

export function judgePrompt(scorerLabel: string, run: Pick<Run, "id" | "agent_name" | "state">): string {
  const agent = run.agent_name || "unknown";
  return `Score the run ${run.id} (agent ${agent}, state ${run.state}) with the scorer ${JSON.stringify(scorerLabel)}. Read \`transcript.md\` of the skill \`${JUDGE_SKILL_NAME}\` first. Then give your answer as the JSON object of your instructions.`;
}

export interface JudgeRequestInput {
  factoryId: string;
  name: string;
  rubric: string;
  labels: readonly ScorerLabel[];
  exec: JudgeExec;
  scored: Run;
  includeDescendants: boolean;
  refs?: RepoAtCommit[];
}

/** Builds the request of a judge run (judgeRequest in Go). */
export async function judgeRequest(runs: RunsPort, input: JudgeRequestInput): Promise<JudgeRunRequest> {
  const { exec } = input;
  if (!exec.harness) {
    throw new OfError(
      "invalid_request",
      `the scorer ${JSON.stringify(input.name)} has no harness (set harness or model on the scorer, scorerDefaults or agentDefaults)`,
    );
  }
  const transcript = await buildTranscript(runs, input.scored, input.includeDescendants, 0, DEFAULT_MAX_JUDGE_FILE_BYTES);
  let refs = input.refs ?? [];
  if (refs.length === 0) {
    refs = (await runs.loadStartCommits(input.scored.id)).map((s) => ({ codeForge: s.codeForge, owner: s.owner, repo: s.repo, ref: s.ref }));
  }
  const req: JudgeRunRequest = {
    factoryId: input.factoryId,
    name: clip("scorer-" + input.name, 64),
    instructions: judgeInstructions(input.name, input.rubric, input.labels),
    prompt: judgePrompt(input.name, input.scored),
    harness: exec.harness,
    files: { "transcript.md": transcript },
    startingRepoRefs: refs,
  };
  if (exec.machine) req.machine = exec.machine;
  if (exec.secrets) req.secrets = exec.secrets;
  if (exec.mcp_servers) req.mcpServers = exec.mcp_servers;
  return req;
}

export const isJudgeRun = (r: Pick<Run, "metadata" | "triggered_by">): boolean =>
  !!r.metadata?.["judge"] || (r.triggered_by ?? "").startsWith("scorer:");

/** Reads the label and the rationale from a finished judge run. */
export async function judgeAnswer(runs: Pick<RunsPort, "listMessages">, judge: Run): Promise<{ label: string; rationale: string }> {
  switch (judge.state) {
    case "cancelled":
      throw new Error("the judge run was cancelled");
    case "error":
    case "failed": {
      const msg = judge.error || judge.state;
      const a = parseJudgeAnswer(judge.result ?? "");
      if (a && judge.state === "failed") return a;
      throw new Error(`the judge run ended with ${judge.state}: ${msg}`);
    }
  }
  const a = parseJudgeAnswer(judge.result ?? "");
  if (a) return a;
  const msgs = await runs.listMessages(judge.id);
  for (let i = msgs.length - 1; i >= 0; i--) {
    if (msgs[i]!.role !== "assistant") continue;
    const m = parseJudgeAnswer(msgs[i]!.content);
    if (m) return m;
  }
  throw new Error("the final message has no JSON object with a label");
}

/** The usage of a run and all its descendants (breadth first, bounded). */
export async function treeUsage(runs: Pick<RunsPort, "listChildren">, run: Pick<Run, "id" | "usage">): Promise<Usage> {
  const u: Usage = { ...(run.usage ?? { input_tokens: 0, output_tokens: 0, cost_usd: 0, duration_ms: 0 }) };
  const queue = [run.id];
  const seen = new Set(queue);
  let n = 0;
  while (queue.length > 0 && n < MAX_DESCENDANTS) {
    const id = queue.shift()!;
    for (const c of await runs.listChildren(id)) {
      if (seen.has(c.id)) continue;
      seen.add(c.id);
      n++;
      u.input_tokens += c.usage?.input_tokens ?? 0;
      u.output_tokens += c.usage?.output_tokens ?? 0;
      u.cost_usd += c.usage?.cost_usd ?? 0;
      u.duration_ms += c.usage?.duration_ms ?? 0;
      queue.push(c.id);
    }
  }
  return u;
}
