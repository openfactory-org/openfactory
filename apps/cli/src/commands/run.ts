import type { OpenFactoryClient, Run, RunMessage } from "@openfactory/api-client";
import { runFinished, runWaitsForInput } from "@openfactory/api-client";
import { ago, emit, localClock, rfc3339Local, table, trunc } from "../format.ts";
import { enc, isJSON, makeClient, resolveFactory } from "../client.ts";
import { exactArgs, minArgs, noArgs, type Command, type Env, type Writer } from "../framework.ts";

export function printMessage(w: Writer, m: RunMessage): void {
  const ts = localClock(new Date(m.created_at ?? Date.now()));
  let content = m.content.replace(/\n+$/, "");
  if (m.role === "tool_call" || m.role === "tool_result" || m.role === "log") content = trunc(content, 200);
  w.write(`${ts} ${m.role.padEnd(11)} ${content}\n`);
}

const sleepMs = (ms: number, signal: AbortSignal): Promise<void> =>
  new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(t);
        resolve();
      },
      { once: true },
    );
  });

/** Prints run messages; with follow it polls until the run is idle or finished. */
export async function streamLogs(c: OpenFactoryClient, env: Env, runId: string, follow: boolean): Promise<void> {
  let after = 0;
  for (;;) {
    for (const m of await c.runMessages(runId, after)) {
      printMessage(env.stdout, m);
      if (m.seq > after) after = m.seq;
    }
    if (!follow) return;
    const r = await c.request<Run>("GET", `runs/${enc(runId)}`);
    if (runFinished(r.state) || runWaitsForInput(r.state)) {
      // Drain anything written after the last poll.
      try {
        for (const m of await c.runMessages(runId, after)) printMessage(env.stdout, m);
      } catch {
        // The state line below still tells the outcome.
      }
      env.stdout.write(`-- run ${runId} is ${r.state}\n`);
      return;
    }
    if (env.signal.aborted) return;
    await sleepMs(1000, env.signal);
    if (env.signal.aborted) return;
  }
}

const listCommand: Command = {
  use: "list",
  short: "List recent runs",
  args: noArgs,
  flags: [
    { name: "factory", type: "string", usage: "only runs of this factory" },
    { name: "limit", type: "int", default: 20, usage: "maximum runs to show" },
    {
      name: "state",
      type: "string",
      usage: "comma-separated states (queued,pending,claimed,running,blocked,succeeded,failed,error,cancelled)",
    },
    { name: "work-item", type: "string", usage: "only runs of this work item" },
  ],
  async run({ flags, globals, env }) {
    const c = makeClient(globals, env);
    const limit = flags.int("limit");
    const query: Record<string, string | undefined> = {};
    if (flags.string("work-item") !== "") query["work_item_id"] = flags.string("work-item");
    if (flags.string("factory") !== "") query["factory_id"] = (await resolveFactory(c, flags.string("factory"))).uid;
    if (flags.string("state") !== "") query["state"] = flags.string("state").toUpperCase();
    const runs: Run[] = [];
    for await (const r of c.list<Run>("runs", query, limit).items()) {
      if (runs.length >= limit) break;
      runs.push(r);
    }
    if (isJSON(globals)) return emit(env.stdout, runs);
    table(
      env.stdout,
      ["RUN", "STATE", "NAME", "AGENT", "CREATED", "TITLE"],
      runs.map((r) => [
        r.run_id,
        r.state,
        r.name ?? "",
        r.agent_name ?? "",
        ago(r.created_at, env.now()),
        trunc(r.title, 50),
      ]),
    );
  },
};

const getCommand: Command = {
  use: "get <run>",
  short: "Show a run",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    const c = makeClient(globals, env);
    const r = await c.request<Run>("GET", `runs/${enc(args[0] ?? "")}`);
    if (isJSON(globals)) return emit(env.stdout, r);
    let out = `Run:      ${r.run_id}\nState:    ${r.state}\nAgent:    ${r.agent_name ?? ""} (${r.name ?? ""})\nCreated:  ${rfc3339Local(new Date(r.created_at))}\nURL:      ${r.run_url ?? ""}\n`;
    if (r.parent_run_id) out += `Parent:   ${r.parent_run_id}\n`;
    const u = r.usage;
    out += `Usage:    ${u?.input_tokens ?? 0} in / ${u?.output_tokens ?? 0} out tokens, $${(u?.cost_usd ?? 0).toFixed(4)}\n`;
    if (r.error) out += `Error:    ${r.error}\n`;
    if (r.result) out += `\n${r.result}\n`;
    env.stdout.write(out);
  },
};

const logsCommand: Command = {
  use: "logs <run>",
  short: "Print a run's transcript",
  args: exactArgs(1),
  flags: [{ name: "follow", short: "f", type: "boolean", usage: "keep streaming until the run finishes its turn" }],
  async run({ args, flags, globals, env }) {
    await streamLogs(makeClient(globals, env), env, args[0] ?? "", flags.bool("follow"));
  },
};

const followupCommand: Command = {
  use: "followup <run> <message>",
  short: "Send a follow-up message to a run",
  args: minArgs(2),
  flags: [{ name: "follow", short: "f", type: "boolean", usage: "stream the transcript afterwards" }],
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const r = await c.request<Run>("POST", `runs/${enc(args[0] ?? "")}/followups`, {
      body: { message: args.slice(1).join(" ") },
    });
    env.stdout.write(`Follow-up queued for ${r.run_id} (${r.state})\n`);
    if (flags.bool("follow")) await streamLogs(c, env, r.run_id, true);
  },
};

const cancelCommand: Command = {
  use: "cancel <run>",
  short: "Cancel a run and its child runs",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    const c = makeClient(globals, env);
    await c.request("POST", `runs/${enc(args[0] ?? "")}/cancel`, { body: { cascade: true } });
    env.stdout.write(`Cancelled ${args[0] ?? ""}\n`);
  },
};

export const runCommand: Command = {
  use: "run",
  aliases: ["runs"],
  short: "Inspect and control agent runs",
  subcommands: [listCommand, getCommand, logsCommand, followupCommand, cancelCommand],
};
