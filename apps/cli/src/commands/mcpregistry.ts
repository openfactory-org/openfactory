import { ago, emit, table } from "../format.ts";
import { enc, isJSON, makeClient } from "../client.ts";
import { exactArgs, noArgs, type Command } from "../framework.ts";

interface RegisteredMCPServer {
  id: string;
  name: string;
  url?: string;
  command?: string;
  args?: string[];
  connection?: string;
  oauth?: { status?: string } | null;
  updated_at?: string;
  warnings?: string[];
}

interface Builtin {
  managed_id: string;
  configured: boolean;
}

/** A short description of how the server is reached. */
export function mcpMode(m: RegisteredMCPServer): string {
  if (m.oauth) return "oauth, " + (m.oauth.status ?? "").replaceAll("_", " ");
  if (m.command) return "command";
  if (m.connection === "proxy") return "proxied";
  return "direct";
}

/** Parses NAME=VALUE flags. Returns undefined for no flags. */
export function keyValues(flag: string, items: string[]): Record<string, string> | undefined {
  if (items.length === 0) return undefined;
  const out: Record<string, string> = {};
  for (const it of items) {
    const i = it.indexOf("=");
    if (i < 0 || it.slice(0, i).trim() === "") throw new Error(`${flag} ${JSON.stringify(it)}: want NAME=VALUE`);
    out[it.slice(0, i).trim()] = it.slice(i + 1);
  }
  return out;
}

const listCommand: Command = {
  use: "list",
  short: "List registered and built-in MCP servers",
  args: noArgs,
  async run({ globals, env }) {
    const c = makeClient(globals, env);
    const list = await c.list<RegisteredMCPServer>("mcp-servers").collect();
    const first = await c.request<{ builtin?: Builtin[] }>("GET", "mcp-servers");
    const builtin = first.builtin ?? [];
    if (isJSON(globals)) return emit(env.stdout, { mcp_servers: list, builtin });
    const rows = list.map((m) => {
      const transport = m.command ? [m.command, ...(m.args ?? [])].join(" ").trim() : (m.url ?? "");
      return [m.name, transport, mcpMode(m), ago(m.updated_at, env.now())];
    });
    for (const b of builtin)
      rows.push([b.managed_id, "built-in", b.configured ? "available" : "integration not configured", ""]);
    table(env.stdout, ["NAME", "TRANSPORT", "MODE", "UPDATED"], rows);
  },
};

const getCommand: Command = {
  use: "get <name>",
  short: "Show a registered MCP server",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    const c = makeClient(globals, env);
    emit(env.stdout, await c.request("GET", `mcp-servers/${enc(args[0] ?? "")}`));
  },
};

const addCommand: Command = {
  use: "add <name>",
  short: "Register an MCP server",
  example: `  openfactory mcp-registry add sentry --url https://mcp.sentry.dev/mcp \\
    --header 'Authorization=Bearer {{SENTRY_TOKEN}}'
  openfactory mcp-registry add private --url http://10.0.0.5:8080/mcp --direct
  openfactory mcp-registry add notion --url https://mcp.notion.com/mcp --oauth
  openfactory mcp-registry add docs --command npx --arg -y --arg @acme/docs-mcp`,
  args: exactArgs(1),
  flags: [
    { name: "arg", type: "stringArray", usage: "command argument (repeatable)" },
    {
      name: "client-id",
      type: "string",
      usage: "pre-registered OAuth client id (default: dynamic client registration)",
    },
    { name: "client-secret-name", type: "string", usage: "team secret holding the OAuth client secret" },
    { name: "command", type: "string", usage: "executable started inside the sandbox (stdio transport)" },
    { name: "description", type: "string", usage: "description shown in the dashboard" },
    {
      name: "direct",
      type: "boolean",
      usage: "connect from the sandbox, for a server the control plane cannot reach (headers go to the sandbox)",
    },
    {
      name: "env",
      type: "stringArray",
      usage: "environment variable NAME=VALUE for command servers (repeatable; credentials as {{SECRET}})",
    },
    {
      name: "header",
      type: "stringArray",
      usage: "HTTP header NAME=VALUE for url servers (repeatable; credentials as {{SECRET}})",
    },
    { name: "oauth", type: "boolean", usage: "the server uses OAuth (needs the proxy); connect it afterwards" },
    {
      name: "scope",
      type: "stringArray",
      usage: "OAuth scope to request (repeatable; default: the server's advertised scopes)",
    },
    { name: "url", type: "string", usage: "streamable HTTP endpoint (or legacy SSE when the path ends in /sse)" },
  ],
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const body: Record<string, unknown> = { name: args[0] };
    const set = (k: string, v: string): void => {
      if (v !== "") body[k] = v;
    };
    set("description", flags.string("description"));
    set("url", flags.string("url"));
    set("command", flags.string("command"));
    if (flags.list("arg").length > 0) body["args"] = flags.list("arg");
    const headers = keyValues("--header", flags.list("header"));
    if (headers) body["headers"] = headers;
    const envVars = keyValues("--env", flags.list("env"));
    if (envVars) body["env"] = envVars;
    if (flags.bool("direct")) body["connection"] = "direct";
    const scopes = flags.list("scope");
    if (flags.bool("oauth") || flags.string("client-id") !== "" || scopes.length > 0) {
      body["oauth"] = {
        scopes,
        client_id: flags.string("client-id"),
        client_secret_name: flags.string("client-secret-name"),
      };
    }
    const m = await c.request<RegisteredMCPServer>("POST", "mcp-servers", { body });
    if (isJSON(globals)) return emit(env.stdout, m);
    for (const w of m.warnings ?? []) env.stderr.write(`Warning: ${w}\n`);
    let out = `Registered MCP server ${m.name} (${mcpMode(m)}). Reference it with managedId: ${m.name}\n`;
    if (m.oauth) out += `Connect it: openfactory mcp-registry connect ${m.name}\n`;
    env.stdout.write(out);
  },
};

const deleteCommand: Command = {
  use: "delete <name>",
  short: "Delete a registered MCP server and its stored tokens",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    await makeClient(globals, env).request("DELETE", `mcp-servers/${enc(args[0] ?? "")}`);
    env.stdout.write(`Deleted MCP server ${args[0] ?? ""}\n`);
  },
};

const connectCommand: Command = {
  use: "connect <name>",
  short: "Start the OAuth connection of a server",
  long: `Print the provider's authorization URL. Open it in a browser where you are
signed in to the dashboard as the same user as the API key: the provider
redirects back to the server, which stores the tokens.`,
  args: exactArgs(1),
  async run({ args, globals, env }) {
    const out = await makeClient(globals, env).request<{ authorization_url: string }>(
      "POST",
      `mcp-servers/${enc(args[0] ?? "")}/oauth/connect`,
    );
    env.stdout.write(`Open this URL in a browser signed in to the dashboard:\n${out.authorization_url}\n`);
  },
};

const disconnectCommand: Command = {
  use: "disconnect <name>",
  short: "Delete the stored OAuth tokens of a server",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    await makeClient(globals, env).request("POST", `mcp-servers/${enc(args[0] ?? "")}/oauth/disconnect`);
    env.stdout.write(`Disconnected MCP server ${args[0] ?? ""}\n`);
  },
};

export const mcpRegistryCommand: Command = {
  use: "mcp-registry",
  short: "Manage the team MCP server registry",
  long: `Register MCP servers once per team and reference them from agents with
mcpServers.<name>.managedId: <server name>. Credentials go in secrets and are
referenced as {{SECRET_NAME}}. A url server is reached through the
control-plane proxy by default, so its headers and OAuth tokens never enter
the sandbox. Use --direct only for a server that the control plane cannot
reach: its headers then go to the sandbox.`,
  subcommands: [listCommand, getCommand, addCommand, deleteCommand, connectCommand, disconnectCommand],
};
