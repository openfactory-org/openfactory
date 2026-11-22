import { OfError, doName, partitionOf } from "@openfactory/core";

/** The BenchmarkDO name of a benchmark run ID (`bench_<token>...`). */
export function benchmarkDoName(teamId: string, benchId: string): string {
  const token = partitionOf(benchId);
  if (token[0] !== "b") throw new OfError("resource_not_found", `benchmark run ${benchId} not found`);
  return doName.benchmark(teamId, token);
}
