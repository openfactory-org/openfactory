// REST handlers: benchmarks. C0 stubs that answer 501. Owner: WS13.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/benchmarks: List benchmark suites (owner DO: TeamDO). */
export const listBenchmarkSuites: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/benchmarks/{suite}/runs: List runs of a suite (owner DO: IndexDO (read model)). */
export const listSuiteBenchmarkRuns: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/benchmarks/{suite}/runs: Run a benchmark suite (owner DO: BenchmarkDO (new)). */
export const startBenchmark: Handler = notImplementedHandler;

/** GET /api/v1/factories/{uid}/benchmark-runs: List benchmark runs (owner DO: IndexDO (read model)). */
export const listBenchmarkRuns: Handler = notImplementedHandler;

/** GET /api/v1/benchmark-runs/{id}: Get a benchmark run (owner DO: BenchmarkDO). */
export const getBenchmarkRun: Handler = notImplementedHandler;

/** POST /api/v1/benchmark-runs/{id}/cancel: Cancel a benchmark run (owner DO: BenchmarkDO). */
export const cancelBenchmarkRun: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/benchmarks/{suite}/tasks: Make a benchmark task from a finished run (owner DO: WorkDO read → TeamDO). */
export const createBenchmarkTaskFromRun: Handler = notImplementedHandler;
