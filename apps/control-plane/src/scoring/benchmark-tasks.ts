// A benchmark task from a finished run: port of benchmark_tasks.go.
import { parseDocument, stringify as yamlStringify, YAMLSeq, Scalar, isMap, isSeq, type Document } from "yaml";
import { OfError, type BenchmarkSuite, type BenchmarkTask, type Run } from "@openfactory/core";
import { definitionRoot } from "./improve";
import { findSuite, formatRepoAtCommit } from "./benchmark";
import { finishedForScoring, type RunsPort } from "./types";

export interface TaskFromRunRequest {
  factoryId: string;
  suite: string;
  runId: string;
  slug?: string;
  title?: string;
  /** What a successful trial does. Required. */
  successCriteria: string;
  tags?: string[];
}

export interface TaskFromRun {
  task: BenchmarkTask;
  /** Path of the new task file. */
  path: string;
  /** The changed files: the task file and the suite file with the task in its list. */
  files: Record<string, string>;
  /** The full definition tree with the change. */
  definition: Record<string, string>;
}

const SHA_RE = /^[0-9a-f]{40}$/;

/** A slug from free text: lower case letters and digits separated by single dashes. */
export function slugify(title: string): string {
  let slug = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  if (slug.length > 50) slug = slug.slice(0, 50).replace(/^-+|-+$/g, "");
  return slug;
}

/**
 * Builds a benchmark task from a finished run: the run's prompt, and the
 * commits the run checked out as the start state. It does not apply the change.
 */
export async function makeTaskFromRun(
  runs: Pick<RunsPort, "getRun" | "loadStartCommits">,
  active: { definition: { benchmarks?: BenchmarkSuite[] | null } | null; files: Record<string, string> } | null,
  req: TaskFromRunRequest,
): Promise<TaskFromRun> {
  if (req.successCriteria.trim() === "") throw new OfError("invalid_request", "success_criteria is required");
  const run: Run | null = await runs.getRun(req.runId);
  if (!run) throw new OfError("resource_not_found", `run ${req.runId} not found`);
  if (run.factory_id !== req.factoryId) throw new OfError("resource_not_found", `run ${req.runId} not found`);
  if (!finishedForScoring(run.state)) throw new OfError("conflict", `run ${run.id} is not finished`);
  if (run.prompt.trim() === "") throw new OfError("invalid_request", `run ${run.id} has no prompt`);
  if (!active) throw new OfError("invalid_request", "the factory has no active definition");
  let suiteSlug = req.suite;
  const found = findSuite(active.definition?.benchmarks, req.suite);
  if (found) suiteSlug = found.slug;
  const root = definitionRoot(active.files);
  const suitePath = `${root}benchmarks/${suiteSlug}/suite.yaml`;
  const suiteYaml = active.files[suitePath];
  if (suiteYaml === undefined)
    throw new OfError("resource_not_found", `benchmark suite ${JSON.stringify(req.suite)} not found`);
  const starts = await runs.loadStartCommits(run.id);
  const task: BenchmarkTask = {
    slug: "",
    title: (req.title ?? "").trim(),
    prompt: run.prompt.trim(),
    successCriteria: req.successCriteria.trim(),
    sourceRunId: run.id,
  };
  if (req.tags && req.tags.length > 0) task.tags = req.tags;
  const refs: string[] = [];
  const startingRepoRefs: NonNullable<BenchmarkTask["startingRepoRefs"]> = [];
  for (const st of starts) {
    if (!st.resolved || !SHA_RE.test(st.ref)) {
      throw new OfError(
        "invalid_request",
        `run ${run.id} has no recorded start commit for ${st.owner}/${st.repo} (only runs on the runner path record the commits they check out)`,
      );
    }
    const forge = st.codeForge.toUpperCase();
    if (forge !== "GITHUB" && forge !== "GITLAB") {
      throw new OfError(
        "invalid_request",
        `benchmark tasks support GitHub and GitLab repositories only (${st.owner}/${st.repo} is on ${st.codeForge})`,
      );
    }
    const ref = { codeForge: forge, owner: st.owner, repo: st.repo, ref: st.ref };
    startingRepoRefs.push(ref);
    refs.push(formatRepoAtCommit(ref));
  }
  if (startingRepoRefs.length > 0) task.startingRepoRefs = startingRepoRefs;
  if (task.title === "") {
    const name = run.name || run.agent_name || "";
    task.title = `${name} ${run.id}`.trim();
  }
  let slug = (req.slug ?? "").trim();
  if (slug === "") {
    slug = slugify(task.title);
    if (slug === "") slug = "task-" + run.id.toLowerCase();
  }
  task.slug = slug;
  const taskPath = `${root}benchmarks/${suiteSlug}/tasks/${slug}.yaml`;
  if (taskPath in active.files)
    throw new OfError("conflict", `task ${JSON.stringify(slug)} already exists in suite ${JSON.stringify(suiteSlug)}`);
  const doc: Record<string, unknown> = {
    title: task.title,
    prompt: task.prompt,
    successCriteria: task.successCriteria,
    sourceRunId: task.sourceRunId,
  };
  if (req.tags && req.tags.length > 0) doc["tags"] = req.tags;
  if (refs.length > 0) doc["startingRepoRefs"] = refs;
  const taskYaml = yamlStringify(doc, { sortMapEntries: true });
  const files = { [taskPath]: taskYaml, [suitePath]: addSuiteTask(suiteYaml, slug) };
  return { task, path: taskPath, files, definition: { ...active.files, ...files } };
}

/** Appends slug to the tasks list of a suite.yaml document. */
export function addSuiteTask(content: string, slug: string): string {
  const doc: Document = parseDocument(content);
  if (doc.errors.length > 0) throw new OfError("invalid_request", `suite.yaml: ${doc.errors[0]!.message}`);
  if (!isMap(doc.contents)) throw new OfError("invalid_request", "suite.yaml is not a mapping");
  const tasks = doc.contents.get("tasks", true);
  if (isSeq(tasks)) {
    tasks.add(new Scalar(slug));
  } else if (tasks !== undefined) {
    const seq = new YAMLSeq();
    seq.add(new Scalar(slug));
    doc.contents.set("tasks", seq);
  } else {
    const seq = new YAMLSeq();
    seq.add(new Scalar(slug));
    doc.contents.set("tasks", seq);
  }
  return doc.toString({ indent: 2 });
}
