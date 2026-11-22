import { compile } from "@openfactory/definition";
import { loadDir } from "@openfactory/definition/node";
import { ago, emit, parseKV, table } from "../format.ts";
import { enc, explain, isJSON, makeClient, resolveFactory, type Factory } from "../client.ts";
import { CliError, exactArgs, minArgs, noArgs, type Command, type RunContext } from "../framework.ts";
import { parseRepoSpec } from "../repo.ts";
import { formatValidationError } from "./local.ts";
import { streamLogs } from "./run.ts";

interface Agent {
  name: string;
  type: string;
  harness?: { type?: string; model?: string };
}

interface Run {
  run_id: string;
  work_item_id?: string;
  run_url?: string;
}

interface FactoryCreated {
  factory: Factory;
}

function createdLine(f: Factory): string {
  return `Created factory ${f.name} (@${f.alias}): ${f.uid}\n`;
}

const listCommand: Command = {
  use: "list",
  short: "List factories",
  args: noArgs,
  async run({ globals, env }) {
    const c = makeClient(globals, env);
    const fs = await c.list<Factory>("factories").collect();
    if (isJSON(globals)) return emit(env.stdout, fs);
    table(
      env.stdout,
      ["UID", "NAME", "ALIAS", "SOURCE", "UPDATED"],
      fs.map((f) => [f.uid, f.name, "@" + f.alias, f.source, ago(f.updated_at, env.now())]),
    );
  },
};

const getCommand: Command = {
  use: "get <factory>",
  short: "Show a factory (UID, alias or name)",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    const c = makeClient(globals, env);
    const f = await resolveFactory(c, args[0] ?? "");
    if (isJSON(globals)) return emit(env.stdout, f);
    let agents: Agent[] = [];
    try {
      agents = await c.list<Agent>(`factories/${enc(f.uid)}/agents`).collect();
    } catch {
      // The agent list is a courtesy: show the factory without it.
    }
    let out = `UID:      ${f.uid}\nName:     ${f.name}\nAlias:    @${f.alias}\nSource:   ${f.source}\n`;
    if (f.description) out += `About:    ${f.description}\n`;
    env.stdout.write(out);
    if (agents.length > 0) {
      env.stdout.write("\nAgents:\n");
      table(
        env.stdout,
        ["NAME", "TYPE", "HARNESS", "MODEL"],
        agents.map((a) => [a.name, a.type, a.harness?.type ?? "", a.harness?.model ?? ""]),
      );
    }
  },
};

const createCommand: Command = {
  use: "create",
  short: "Create a factory (default agents, or the definition in --from)",
  long: `Create a factory. Without --from, the server generates the default
definition (foreman plus the default stage agents) for --repo repositories.
With --from DIR, the definition tree in DIR is uploaded. --source git binds the
factory to a definition repository instead (--git-repo, --git-branch, --git-path).`,
  args: noArgs,
  flags: [
    { name: "agents", type: "strings", usage: "default stage agents to include" },
    { name: "alias", type: "string", usage: "foreman @alias" },
    { name: "description", type: "string", usage: "description" },
    { name: "from", type: "string", usage: "upload the definition tree in this directory" },
    { name: "git-branch", type: "string", usage: "production branch for --source git (default main)" },
    { name: "git-path", type: "string", usage: "directory holding factory.yaml in the definition repository" },
    {
      name: "git-repo",
      type: "string",
      usage: "definition repository for --source git (e.g. github.com/acme/factory)",
    },
    { name: "model", type: "string", usage: "default model for generated agents" },
    { name: "name", type: "string", usage: "factory name" },
    { name: "repo", type: "stringArray", usage: "repository [forge:]owner/name (repeatable)" },
    { name: "source", type: "string", usage: "definition source: managed (default) or git" },
  ],
  async run({ flags, globals, env }) {
    const c = makeClient(globals, env);
    const body: Record<string, unknown> = {};
    const set = (k: string, v: string): void => {
      if (v !== "") body[k] = v;
    };
    set("name", flags.string("name"));
    set("alias", flags.string("alias"));
    set("description", flags.string("description"));
    set("model", flags.string("model"));
    set("source", flags.string("source"));
    set("git_repo", flags.string("git-repo"));
    set("git_branch", flags.string("git-branch"));
    set("git_path", flags.string("git-path"));
    const repos = flags.list("repo").map((r) => {
      const ref = parseRepoSpec(r);
      return { codeForge: ref.codeForge, owner: ref.owner, name: ref.name };
    });
    if (repos.length > 0) body["repositories"] = repos;
    if (flags.changed("agents")) body["agents"] = flags.list("agents");
    const from = flags.string("from");
    if (from !== "") body["files"] = loadDir(from);
    let res: FactoryCreated;
    try {
      res = await c.request<FactoryCreated>("POST", "factories", { body });
    } catch (err) {
      throw explain(err);
    }
    if (isJSON(globals)) return emit(env.stdout, res);
    env.stdout.write(createdLine(res.factory));
  },
};

const applyCommand: Command = {
  use: "apply <dir>",
  short: "Validate and apply a definition tree to a managed factory",
  long: `Upload the definition tree in dir as a new version of a managed factory.
The target is --factory, otherwise the factory whose alias matches the
definition's alias (or name); a new factory is created when none exists.
Invalid definitions are rejected and the last valid version stays active.`,
  args: exactArgs(1),
  flags: [
    { name: "factory", type: "string", usage: "target factory (UID, alias or name)" },
    { name: "message", short: "m", type: "string", default: "Applied with openfactory CLI", usage: "version message" },
  ],
  async run({ args, flags, globals, env }) {
    const dir = args[0] ?? "";
    const files = loadDir(dir);
    const { definition: def, errors } = compile(files);
    if (errors.length > 0 || def === null) {
      for (const e of errors) env.stderr.write(formatValidationError(dir, e) + "\n");
      throw new CliError("", true);
    }
    const c = makeClient(globals, env);
    let ref = flags.string("factory");
    const explicit = ref !== "";
    if (!explicit) ref = def.alias || def.name;
    let f: Factory;
    try {
      f = await resolveFactory(c, ref);
    } catch (err) {
      if (explicit || !(err instanceof CliError)) throw err;
      let res: FactoryCreated;
      try {
        res = await c.request<FactoryCreated>("POST", "factories", { body: { files } });
      } catch (e) {
        throw explain(e);
      }
      env.stdout.write(createdLine(res.factory));
      return;
    }
    let out: { version: { id: string } };
    try {
      out = await c.request("PUT", `factories/${enc(f.uid)}/definition`, {
        body: { files, message: flags.string("message") },
      });
    } catch (err) {
      throw explain(err);
    }
    if (isJSON(globals)) return emit(env.stdout, out.version);
    env.stdout.write(`Applied version ${out.version.id} to ${f.name} (@${f.alias})\n`);
  },
};

/** The dispatch command shared by "factory run" and "work-item send". */
export function dispatchCommand(use: string, short: string): Command {
  return {
    use,
    short,
    long: short + `. The prompt may be several words; "-" reads it from stdin.`,
    args: minArgs(2),
    flags: [
      { name: "agent", type: "string", usage: "agent to start (default: the foreman)" },
      { name: "follow", short: "f", type: "boolean", usage: "stream the run transcript" },
      { name: "metadata", type: "stringArray", usage: "run metadata key=value (repeatable)" },
      { name: "title", type: "string", usage: "work item title" },
    ],
    run: dispatch,
  };
}

async function dispatch({ args, flags, globals, env }: RunContext): Promise<void> {
  const c = makeClient(globals, env);
  const f = await resolveFactory(c, args[0] ?? "");
  let prompt = args.slice(1).join(" ");
  if (prompt === "-") prompt = await env.readStdin();
  const metadata = parseKV(flags.list("metadata"), "--metadata");
  const body: Record<string, unknown> = { prompt };
  if (flags.string("title") !== "") body["title"] = flags.string("title");
  if (flags.string("agent") !== "") body["agent"] = flags.string("agent");
  if (metadata) body["metadata"] = metadata;
  let r: Run;
  try {
    r = await c.request<Run>("POST", `factories/${enc(f.uid)}/runs`, { body });
  } catch (err) {
    throw explain(err);
  }
  const follow = flags.bool("follow");
  if (isJSON(globals) && !follow) return emit(env.stdout, r);
  env.stdout.write(`Started run ${r.run_id} (work item ${r.work_item_id ?? ""})\n${r.run_url ?? ""}\n`);
  if (follow) await streamLogs(c, env, r.run_id, true);
}

export const factoryCommand: Command = {
  use: "factory",
  aliases: ["factories"],
  short: "Manage factories",
  subcommands: [
    listCommand,
    getCommand,
    createCommand,
    applyCommand,
    dispatchCommand("run <factory> <prompt>", "Send a work item to a factory's foreman"),
  ],
};
