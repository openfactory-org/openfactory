import { isConflict, type OpenFactoryClient } from "@openfactory/api-client";
import { ago, emit, table } from "../format.ts";
import { enc, isJSON, makeClient, resolveFactory } from "../client.ts";
import { exactArgs, noArgs, rangeArgs, type Command, type Flags, type Writer } from "../framework.ts";

interface Secret {
  name: string;
  type: string;
  factory_id?: string;
  updated_at?: string;
}

export interface RewrapResult {
  rewrapped: number;
  skipped: number;
  key_id?: string;
}

/** Prints the counts of a rewrap. */
export function printRewrap(out: Writer, r: RewrapResult): void {
  out.write(`rewrapped: ${r.rewrapped}\nalready current: ${r.skipped}\n`);
  if (r.key_id) out.write(`key: ${r.key_id}\n`);
}

async function scope(c: OpenFactoryClient, flags: Flags): Promise<string> {
  const ref = flags.string("factory");
  return ref === "" ? "" : (await resolveFactory(c, ref)).uid;
}

const query = (fid: string): Record<string, string | undefined> => (fid === "" ? {} : { factory_id: fid });

const setCommand: Command = {
  use: "set <name> [value]",
  short: "Create or update a secret (value from the argument, or stdin when omitted or -)",
  args: rangeArgs(1, 2),
  flags: [
    {
      name: "type",
      type: "string",
      default: "generic",
      usage: "secret type: generic, anthropic_api_key, openai_api_key, docker_registry, ...",
    },
  ],
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const fid = await scope(c, flags);
    const name = args[0] ?? "";
    const value =
      args.length === 2 && args[1] !== "-" ? (args[1] ?? "") : (await env.readStdin()).replace(/[\r\n]+$/, "");
    let s: Secret;
    try {
      s = await c.request<Secret>("POST", "secrets", {
        body: { name, type: flags.string("type"), value, factory_id: fid },
      });
    } catch (err) {
      if (!isConflict(err)) throw err;
      s = await c.request<Secret>("PUT", `secrets/${enc(name)}`, { query: query(fid), body: { value } });
    }
    env.stdout.write(`Saved secret ${s.name} (${s.type})\n`);
  },
};

const listCommand: Command = {
  use: "list",
  short: "List secrets (names only)",
  args: noArgs,
  async run({ flags, globals, env }) {
    const c = makeClient(globals, env);
    const list = await c.list<Secret>("secrets", query(await scope(c, flags))).collect();
    if (isJSON(globals)) return emit(env.stdout, list);
    table(
      env.stdout,
      ["NAME", "TYPE", "SCOPE", "UPDATED"],
      list.map((s) => [s.name, s.type, s.factory_id ? s.factory_id : "team", ago(s.updated_at, env.now())]),
    );
  },
};

const deleteCommand: Command = {
  use: "delete <name>",
  short: "Delete a secret",
  args: exactArgs(1),
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const fid = await scope(c, flags);
    await c.request("DELETE", `secrets/${enc(args[0] ?? "")}`, { query: query(fid) });
    env.stdout.write(`Deleted secret ${args[0] ?? ""}\n`);
  },
};

const rewrapCommand: Command = {
  use: "rewrap",
  short: "Wrap the data keys of all secrets again with the current secret key",
  long: `Ask the server to read each stored secret, OAuth token of an MCP server and
personal token of a user, unwrap its data key with any known secret key and
wrap it again with the current key. Run it after you move the old key to
the previous-key setting of the control plane. Remove the previous key only
when the command reports success. It needs the admin role. It exits with
code 1 when the server reports an error.`,
  args: noArgs,
  async run({ globals, env }) {
    const c = makeClient(globals, env);
    const res = await c.request<RewrapResult>("POST", "secrets/rewrap");
    if (isJSON(globals)) return emit(env.stdout, res);
    printRewrap(env.stdout, res);
  },
};

export const secretCommand: Command = {
  use: "secret",
  aliases: ["secrets"],
  short: "Manage encrypted secrets",
  persistentFlags: [{ name: "factory", type: "string", usage: "factory scope (default: team scope)" }],
  subcommands: [rewrapCommand, setCommand, listCommand, deleteCommand],
};
