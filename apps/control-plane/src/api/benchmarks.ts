// REST handlers: benchmarks. Owner: WS13.
import { newRoutedId, newToken, doName, type BenchmarkRun } from "@openfactory/core";
import { requireRole } from "../auth/roles";
import { callMeta, callTeam, errorResponse, json, opParams, readJson, teamApi, teamOf, type Handler } from "../http/handler";
import { bodyObject, optString, str } from "../lib/body";
import { makeTaskFromRun } from "../scoring/benchmark-tasks";
import { unwrapOp, workEngine, workRunsPort } from "../scoring/clients";
import { benchmarkDoName } from "../scoring/names";
import type { BenchmarkPlan } from "../do/benchmark/index";
import { factoryAccess, indexList } from "./measure";

/** GET /api/v1/factories/{uid}/benchmarks. */
export const listBenchmarkSuites: Handler = teamApi();

async function listRuns(env: Parameters<Handler>[1], ctx: Parameters<Handler>[2], suite: string): Promise<Response> {
  const f = await factoryAccess(env, ctx);
  const q = ctx.url.searchParams;
  const params: Record<string, string> = { factory_id: f.factory_id };
  for (const k of ["limit", "cursor", "sort_order", "sort_by"]) if (q.get(k)) params[k] = q.get(k)!;
  if (suite) params["suite"] = suite;
  const page = (await indexList(env, ctx)("benchmarkRuns", params)) as { items: BenchmarkRun[]; page_info: unknown };
  const state = q.get("state");
  return json(state ? { ...page, items: page.items.filter((b) => b.state === state) } : page);
}

/** GET /api/v1/factories/{uid}/benchmarks/{suite}/runs. */
export const listSuiteBenchmarkRuns: Handler = (_r, env, ctx) => listRuns(env, ctx, ctx.params["suite"] ?? "");

/** GET /api/v1/factories/{uid}/benchmark-runs. */
export const listBenchmarkRuns: Handler = (_r, env, ctx) => listRuns(env, ctx, ctx.url.searchParams.get("suite") ?? "");

/** POST /api/v1/factories/{uid}/benchmarks/{suite}/runs: TeamDO validates and snapshots, BenchmarkDO runs. */
export const startBenchmark: Handler = async (req, env, ctx) => {
  try {
    const body = await readJson(req);
    const teamId = teamOf(ctx);
    const meta = callMeta(ctx);
    const planned = unwrapOp<{ factoryId: string; plan: BenchmarkPlan; repoSubstitutions: unknown[] }>(
      await env.TEAM.getByName(doName.team(teamId)).api(meta, "scoringBenchmarkPlan", opParams(ctx), body ?? {}),
    );
    const token = newToken("b");
    const id = newRoutedId("bench", token);
    const run = await env.BENCHMARK.getByName(doName.benchmark(teamId, token)).start(meta, { id, ...planned });
    return json(run, 201);
  } catch (e) {
    return errorResponse(e, { instance: ctx.url.pathname, requestId: ctx.requestId });
  }
};

/** GET /api/v1/benchmark-runs/{id}. */
export const getBenchmarkRun: Handler = async (_r, env, ctx) => {
  try {
    return json(await env.BENCHMARK.getByName(benchmarkDoName(teamOf(ctx), ctx.params["id"] ?? "")).get(callMeta(ctx)));
  } catch (e) {
    return errorResponse(e, { instance: ctx.url.pathname, requestId: ctx.requestId });
  }
};

/** POST /api/v1/benchmark-runs/{id}/cancel. */
export const cancelBenchmarkRun: Handler = async (_r, env, ctx) => {
  try {
    return json(await env.BENCHMARK.getByName(benchmarkDoName(teamOf(ctx), ctx.params["id"] ?? "")).cancel(callMeta(ctx)));
  } catch (e) {
    return errorResponse(e, { instance: ctx.url.pathname, requestId: ctx.requestId });
  }
};

/** POST /api/v1/factories/{uid}/benchmarks/{suite}/tasks: a task from a finished run. A managed definition is applied at once. */
export const createBenchmarkTaskFromRun: Handler = async (req, env, ctx) => {
  try {
    const b = bodyObject(await readJson(req, false));
    const teamId = teamOf(ctx);
    const meta = callMeta(ctx);
    const info = unwrapOp<{ factory: { id: string; source: string }; definition: { benchmarks?: never } | null; files: Record<string, string> }>(
      await env.TEAM.getByName(doName.team(teamId)).api(meta, "scoringTaskContext", opParams(ctx), undefined),
    );
    requireRole(ctx.principal, info.factory.id, "editor");
    const runId = str(b, "from_run_id");
    const task = await makeTaskFromRun(workRunsPort(env, teamId), info.definition ? { definition: info.definition, files: info.files } : null, {
      factoryId: info.factory.id,
      suite: ctx.params["suite"] ?? "",
      runId,
      ...(optString(b, "slug") ? { slug: optString(b, "slug")! } : {}),
      ...(optString(b, "title") ? { title: optString(b, "title")! } : {}),
      successCriteria: optString(b, "success_criteria") ?? "",
      ...(Array.isArray(b["tags"]) ? { tags: b["tags"] as string[] } : {}),
    });
    const out: Record<string, unknown> = { task: task.task, files: task.files, applied: false };
    if (info.factory.source !== "git") {
      const v = await workEngine(env, teamId).applyDefinition(info.factory.id, task.definition, meta.principal?.userId ? "user:" + meta.principal.userId : "api", `Add a benchmark task from run ${runId}`);
      out["applied"] = true;
      out["version_id"] = v.id;
    }
    return json(out, 201);
  } catch (e) {
    return errorResponse(e, { instance: ctx.url.pathname, requestId: ctx.requestId });
  }
};

export { callTeam };
