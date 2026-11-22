import { unwrap } from "@openfactory/api-client";
import { ago, emit, table } from "../format.ts";
import { enc, isJSON, makeClient, resolveFactory, type Schemas } from "../client.ts";
import { CliError, exactArgs, type Command } from "../framework.ts";

/** The most repetitions the API and the dashboard accept. */
export const MAX_BENCHMARK_REPETITIONS = 10;

/** Rejects repetitions outside 1..10 before the API is called. */
export function validateRepetitions(n: number): void {
  if (n < 1 || n > MAX_BENCHMARK_REPETITIONS) {
    throw new CliError(`--repetitions must be between 1 and ${MAX_BENCHMARK_REPETITIONS} (got ${n})`);
  }
}

const runCommand: Command = {
  use: "run <factory> <suite>",
  short: "Launch a benchmark suite",
  args: exactArgs(2),
  flags: [
    { name: "config", type: "strings", usage: "configuration names to compare (default: the suite's)" },
    { name: "repetitions", type: "int", default: 1, usage: "repetitions per configuration and task (1 to 10)" },
    { name: "scorer", type: "strings", usage: "additional scorers (ID or slug)" },
  ],
  async run({ args, flags, globals, env }) {
    const reps = flags.int("repetitions");
    validateRepetitions(reps);
    const c = makeClient(globals, env);
    const f = await resolveFactory(c, args[0] ?? "");
    const body: Schemas["StartBenchmarkRequest"] = {
      configurations: flags.list("config").map((name) => ({ name })),
      repetitions: reps,
    };
    if (flags.list("scorer").length > 0) body.scorers = flags.list("scorer");
    const br = await unwrap(
      c.api.POST("/factories/{uid}/benchmarks/{suite}/runs", {
        params: { path: { uid: f.uid, suite: args[1] ?? "" } },
        body,
      }),
    );
    if (isJSON(globals)) return emit(env.stdout, br);
    env.stdout.write(`Started benchmark run ${br.id ?? ""} (${br.state ?? ""})\n`);
  },
};

const listCommand: Command = {
  use: "list <factory>",
  short: "List benchmark suites and recent runs",
  args: exactArgs(1),
  flags: [{ name: "suite", type: "string", usage: "only runs of this suite" }],
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const f = await resolveFactory(c, args[0] ?? "");
    const suites = await c.list<Schemas["BenchmarkSuite"]>(`factories/${enc(f.uid)}/benchmarks`).collect();
    const suite = flags.string("suite");
    const runsPath =
      suite === "" ? `factories/${enc(f.uid)}/benchmark-runs` : `factories/${enc(f.uid)}/benchmarks/${enc(suite)}/runs`;
    const runs = await c.list<Schemas["BenchmarkRun"]>(runsPath).collect();
    if (isJSON(globals)) return emit(env.stdout, { suites, runs });
    table(
      env.stdout,
      ["SUITE", "NAME", "AGENT", "CONFIGS", "TASKS"],
      suites.map((s) => [
        s.slug ?? "",
        s.name ?? "",
        s.agent ?? "",
        String((s.configurations ?? []).length),
        String((s.tasks ?? []).length),
      ]),
    );
    if (runs.length > 0) {
      env.stdout.write("\n");
      table(
        env.stdout,
        ["RUN", "SUITE", "STATE", "CREATED"],
        runs.map((r) => [r.id ?? "", r.suite ?? "", r.state ?? "", ago(r.created_at, env.now())]),
      );
    }
  },
};

export const benchmarkCommand: Command = {
  use: "benchmark",
  aliases: ["benchmarks"],
  short: "Run and inspect benchmark suites",
  subcommands: [runCommand, listCommand],
};
