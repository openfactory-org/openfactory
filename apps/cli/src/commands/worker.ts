import { ago, emit, table } from "../format.ts";
import { isJSON, makeClient, type Schemas } from "../client.ts";
import { noArgs, type Command } from "../framework.ts";
import { forwardToRuntime } from "./runtime.ts";

const listCommand: Command = {
  use: "list",
  short: "List registered remote workers",
  args: noArgs,
  async run({ globals, env }) {
    const c = makeClient(globals, env);
    const ws = await c.list<Schemas["Worker"]>("workers").collect();
    if (isJSON(globals)) return emit(env.stdout, ws);
    table(
      env.stdout,
      ["ID", "STATUS", "KIND", "ACTIVE", "VERSION", "HEARTBEAT"],
      ws.map((w) => {
        const limit = w.slots ?? w.capacity ?? 0;
        const used = w.slots_used ?? w.active ?? 0;
        return [
          w.id ?? "",
          w.status ?? "",
          w.kind ?? w.provider ?? "",
          `${used}/${limit > 0 ? String(limit) : "unlimited"}`,
          w.version ?? "",
          ago(w.last_heartbeat ?? w.last_seen, env.now()),
        ];
      }),
    );
  },
};

export const workerCommand: Command = {
  use: "worker",
  short: "List remote workers, or run one (through openfactory-runtime)",
  long: `Manage remote workers.

"worker list" asks the server for its registered workers. Any other
arguments start a worker: they go to "openfactory-runtime worker", which must
be on PATH. Run "openfactory-runtime worker --help" for its flags.`,
  group: "run",
  subcommands: [listCommand],
  forward: (args, env) => forwardToRuntime("worker", args, env),
};
