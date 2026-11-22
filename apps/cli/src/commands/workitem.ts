import { ago, emit, table, trunc } from "../format.ts";
import { enc, isJSON, makeClient, resolveFactory } from "../client.ts";
import { exactArgs, type Command } from "../framework.ts";
import { dispatchCommand } from "./factory.ts";

interface WorkItem {
  id: string;
  title: string;
  stage: string;
  source?: { provider?: string };
  updated_at?: string;
}

const listCommand: Command = {
  use: "list <factory>",
  short: "List a factory's work items",
  args: exactArgs(1),
  flags: [{ name: "stage", type: "string", usage: "comma-separated stages" }],
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const f = await resolveFactory(c, args[0] ?? "");
    const stage = flags.string("stage");
    const items = await c.list<WorkItem>(`factories/${enc(f.uid)}/work-items`, stage === "" ? {} : { stage }).collect();
    if (isJSON(globals)) return emit(env.stdout, items);
    table(
      env.stdout,
      ["ID", "STAGE", "SOURCE", "UPDATED", "TITLE"],
      items.map((w) => [w.id, w.stage, w.source?.provider ?? "", ago(w.updated_at, env.now()), trunc(w.title, 60)]),
    );
  },
};

const getCommand: Command = {
  use: "get <id>",
  short: "Show a work item with its timeline",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    const c = makeClient(globals, env);
    emit(env.stdout, await c.request("GET", `work-items/${enc(args[0] ?? "")}`));
  },
};

const completeCommand: Command = {
  use: "complete <id>",
  short: "Mark a work item complete",
  args: exactArgs(1),
  flags: [{ name: "note", type: "string", usage: "closing note" }],
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const w = await c.request<WorkItem>("POST", `work-items/${enc(args[0] ?? "")}/complete`, {
      body: { note: flags.string("note") },
    });
    env.stdout.write(`Work item ${w.id} is ${w.stage}\n`);
  },
};

export const workItemCommand: Command = {
  use: "work-item",
  short: "Work items: list, inspect, send and complete work items",
  subcommands: [
    listCommand,
    getCommand,
    dispatchCommand("send <factory> <prompt>", "Send a new work item to a factory"),
    completeCommand,
  ],
};
