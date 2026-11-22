import { McpBridge } from "../mcp-bridge.ts";
import { CliError, noArgs, type Command } from "../framework.ts";

export const mcpCommand: Command = {
  use: "mcp",
  short: "Bridge the server's Factory MCP to stdio for local coding agents",
  long: `Run a stdio MCP server that forwards every message to the Factory MCP of an
Open Factory server (streamable HTTP at <server>/mcp), authenticating with
your API key. Use it to give a local coding agent the factory tools
(list_factories, send_work_item, get_work_item, ...). Example MCP client configuration:

  {"mcpServers": {"factory": {"command": "openfactory", "args": ["mcp"],
    "env": {"OF_SERVER": "https://factory.example.com", "OF_API_KEY": "of_..."}}}}`,
  group: "run",
  args: noArgs,
  flags: [{ name: "path", type: "string", default: "/mcp", usage: "path of the Factory MCP endpoint on the server" }],
  async run({ flags, globals, env }) {
    if (globals.apiKey === "") throw new CliError("no API key: pass --api-key or set OF_API_KEY");
    const bridge = new McpBridge({
      url: globals.server.replace(/\/+$/, "") + flags.string("path"),
      apiKey: globals.apiKey,
      fetch: env.fetch,
      out: env.stdout,
      errLog: env.stderr,
      signal: env.signal,
    });
    await bridge.run(env.stdin);
  },
};
