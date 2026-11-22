// The command tree of the openfactory CLI.

import { App, CliError, type Command, type Env, type FlagSpec, type Globals, noArgs } from "./framework.ts";
import { bootstrapCommand } from "./commands/bootstrap.ts";
import { benchmarkCommand } from "./commands/benchmark.ts";
import { factoryCommand } from "./commands/factory.ts";
import { inboxCommand } from "./commands/inbox.ts";
import { initCommand, schemaCommand, validateCommand } from "./commands/local.ts";
import { mcpCommand } from "./commands/mcp.ts";
import { mcpRegistryCommand } from "./commands/mcpregistry.ts";
import { runCommand } from "./commands/run.ts";
import { netguardCommand, runnerCommand, serveCommand, vmInitCommand } from "./commands/runtime.ts";
import { secretCommand } from "./commands/secret.ts";
import { webhookCommand } from "./commands/webhook.ts";
import { workerCommand } from "./commands/worker.ts";
import { workItemCommand } from "./commands/workitem.ts";
import { VERSION } from "./version.ts";

/** The default server: the URL of `wrangler dev`. */
export const DEFAULT_SERVER = "http://localhost:8787";

// The help text must never show the value of OF_API_KEY, and OF_SERVER can
// hold a password in its URL. So the flags have fixed defaults, and the
// environment sets the values after the flags are defined (see globalsFrom).
const globalFlags: FlagSpec[] = [
  { name: "api-key", type: "string", usage: "API key (env OF_API_KEY)" },
  {
    name: "output",
    short: "o",
    type: "string",
    default: "text",
    usage: "output format for client commands: text or json",
  },
  { name: "server", type: "string", default: DEFAULT_SERVER, usage: "server base URL (env OF_SERVER)" },
];

const versionCommand: Command = {
  use: "version",
  short: "Print the version",
  long: "Print the version of the openfactory command-line client. --short prints only the version.",
  args: noArgs,
  flags: [{ name: "short", type: "boolean", usage: "print only the version" }],
  async run({ flags, env }) {
    env.stdout.write(
      flags.bool("short")
        ? `${VERSION}\n`
        : `openfactory ${VERSION} node/${process.versions.node} ${process.platform}/${process.arch}\n`,
    );
  },
};

const root: Command = {
  use: "openfactory",
  short: "Open Factory: teams of AI coding agents that turn issues, chats and webhooks into reviewed pull requests",
  long: `openfactory talks to an Open Factory control plane through its REST API
(bootstrap, factory, run, work-item, inbox, secret, webhook, benchmark,
mcp-registry, worker), validates factory definitions locally (validate,
schema, init) and bridges the Factory MCP to stdio (mcp). The commands
runner, netguard, vm-init and worker run on sandboxes and worker hosts: they
are part of openfactory-runtime, and this command passes them on. The worker
host commands are worker enroll, worker run, worker status, worker drain and
worker doctor.

Client commands read the server URL and API key from --server/--api-key or the
OF_SERVER and OF_API_KEY environment variables.`,
  persistentFlags: globalFlags,
  subcommands: [
    { ...mcpCommand },
    { ...netguardCommand },
    { ...runnerCommand },
    { ...workerCommand },
    { ...vmInitCommand },
    { ...serveCommand },
    { ...validateCommand, group: "local" },
    { ...schemaCommand, group: "local" },
    { ...initCommand, group: "local" },
    { ...bootstrapCommand, group: "client" },
    { ...factoryCommand, group: "client" },
    { ...runCommand, group: "client" },
    { ...workItemCommand, group: "client" },
    { ...inboxCommand, group: "client" },
    { ...secretCommand, group: "client" },
    { ...mcpRegistryCommand, group: "client" },
    { ...webhookCommand, group: "client" },
    { ...benchmarkCommand, group: "client" },
    versionCommand,
  ],
};

export function createApp(): App {
  return new App(root, [
    { id: "run", title: "Running Open Factory:" },
    { id: "local", title: "Local definition tools:" },
    { id: "client", title: "Client commands (talk to a server):" },
  ]);
}

/** The connection settings: the environment first, flags override them in the framework. */
export function globalsFrom(vars: Record<string, string | undefined>): Globals {
  return { server: vars["OF_SERVER"] || DEFAULT_SERVER, apiKey: vars["OF_API_KEY"] ?? "", output: "text" };
}

/** Runs the CLI with the given arguments (without node and script) and returns the exit code. */
export async function main(argv: string[], env: Env): Promise<number> {
  const app = createApp();
  const globals = globalsFrom(env.vars);
  const code = await app.execute(argv, env, globals);
  return code;
}

export { CliError };
