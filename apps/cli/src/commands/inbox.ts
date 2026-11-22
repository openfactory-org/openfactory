import { ago, emit, table, trunc } from "../format.ts";
import { enc, isJSON, makeClient, resolveFactory } from "../client.ts";
import { exactArgs, minArgs, noArgs, type Command } from "../framework.ts";

interface InboxItem {
  id: string;
  kind: string;
  title: string;
  resolved: boolean;
  resolution?: string;
  created_at?: string;
}

const listCommand: Command = {
  use: "list",
  short: "List the open inbox items for you",
  args: noArgs,
  flags: [
    { name: "all", type: "boolean", usage: "include resolved items" },
    { name: "factory", type: "string", usage: "only items of this factory" },
    { name: "team", type: "boolean", usage: "list the items of all people (editor or admin)" },
  ],
  async run({ flags, globals, env }) {
    const c = makeClient(globals, env);
    const query: Record<string, string | undefined> = {};
    if (flags.string("factory") !== "") query["factory_id"] = (await resolveFactory(c, flags.string("factory"))).uid;
    if (flags.bool("all")) query["include_resolved"] = "true";
    if (flags.bool("team")) query["scope"] = "team";
    const items = await c.list<InboxItem>("inbox", query).collect();
    if (isJSON(globals)) return emit(env.stdout, items);
    table(
      env.stdout,
      ["ID", "KIND", "STATUS", "CREATED", "TITLE"],
      items.map((it) => [
        it.id,
        it.kind,
        it.resolved ? (it.resolution ?? "") : "open",
        ago(it.created_at, env.now()),
        trunc(it.title, 60),
      ]),
    );
  },
};

const unresolveCommand: Command = {
  use: "unresolve <id>",
  short: "Undo a resolve (within 10 minutes, before the run reads the answer)",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    const c = makeClient(globals, env);
    const it = await c.request<InboxItem>("POST", `inbox/${enc(args[0] ?? "")}/unresolve`);
    if (isJSON(globals)) return emit(env.stdout, it);
    env.stdout.write(`Reopened ${it.id}\n`);
  },
};

const resolveCommand: Command = {
  use: "resolve <id> <resolution>",
  short: "Resolve an inbox item (approve, reject, or an answer)",
  long: "Resolve an inbox item. Approvals take approve or reject; questions take the answer text.",
  args: minArgs(2),
  async run({ args, globals, env }) {
    const c = makeClient(globals, env);
    const it = await c.request<InboxItem>("POST", `inbox/${enc(args[0] ?? "")}/resolve`, {
      body: { resolution: args.slice(1).join(" ") },
    });
    env.stdout.write(`Resolved ${it.id}: ${it.resolution ?? ""}\n`);
  },
};

export const inboxCommand: Command = {
  use: "inbox",
  short: "Approvals, questions and handoffs waiting on people",
  subcommands: [listCommand, unresolveCommand, resolveCommand],
};
