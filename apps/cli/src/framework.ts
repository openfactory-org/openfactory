// A small command framework on node:util parseArgs. It keeps the command
// names, flags, help layout, argument errors and exit codes of the Go CLI
// (cobra), so scripts and muscle memory keep working.

import { parseArgs } from "node:util";

export type FlagType = "string" | "boolean" | "int" | "strings" | "stringArray";

export interface FlagSpec {
  name: string;
  short?: string;
  type: FlagType;
  usage: string;
  /** Shown in help as (default ...). Never an environment value. */
  default?: string | number | boolean;
}

/** The parsed values of the flags that apply to one command. */
export class Flags {
  readonly #values: Map<string, unknown>;
  readonly #changed: Set<string>;
  constructor(values: Map<string, unknown>, changed: Set<string>) {
    this.#values = values;
    this.#changed = changed;
  }
  string(name: string): string {
    const v = this.#values.get(name);
    return typeof v === "string" ? v : "";
  }
  bool(name: string): boolean {
    return this.#values.get(name) === true;
  }
  int(name: string): number {
    const v = this.#values.get(name);
    return typeof v === "number" ? v : 0;
  }
  list(name: string): string[] {
    const v = this.#values.get(name);
    return Array.isArray(v) ? (v as string[]) : [];
  }
  changed(name: string): boolean {
    return this.#changed.has(name);
  }
}

export interface Globals {
  server: string;
  apiKey: string;
  output: string;
}

export interface Writer {
  write(chunk: string): void;
}

/** Everything a command needs from the process, so tests can replace it. */
export interface Env {
  stdout: Writer;
  stderr: Writer;
  /** Reads all of stdin as text. */
  readStdin(): Promise<string>;
  /** Streams stdin lines as chunks (for the MCP bridge). */
  stdin: AsyncIterable<string | Uint8Array>;
  vars: Record<string, string | undefined>;
  cwd: string;
  fetch: typeof fetch;
  signal: AbortSignal;
  /** Runs a program with inherited stdio and resolves to its exit code. */
  runProgram(file: string, args: string[]): Promise<number>;
  now(): Date;
}

export interface RunContext {
  args: string[];
  flags: Flags;
  globals: Globals;
  env: Env;
}

/** A failure to print as "error: <message>" and exit with code 1. */
export class CliError extends Error {
  override readonly name = "CliError";
  /** The details are already printed: print nothing more. */
  readonly silent: boolean;
  /** The process exit code (default 1). */
  readonly exitCode: number;
  constructor(message: string, silent = false, exitCode = 1) {
    super(message);
    this.silent = silent;
    this.exitCode = exitCode;
  }
}

export interface Command {
  /** The name followed by the argument synopsis, for example "get <factory>". */
  use: string;
  short: string;
  long?: string;
  example?: string;
  aliases?: string[];
  group?: string;
  hidden?: boolean;
  flags?: FlagSpec[];
  /** Flags that this command and all its subcommands accept. */
  persistentFlags?: FlagSpec[];
  /** Returns an error message for bad positional arguments, or null. */
  args?: (args: string[]) => string | null;
  run?: (ctx: RunContext) => Promise<void>;
  subcommands?: Command[];
  /** Arguments that are not a subcommand go to this handler with no flag parsing. */
  forward?: (args: string[], env: Env) => Promise<void>;
}

export const noArgs = (args: string[]): string | null => (args.length === 0 ? null : `unknown command "${args[0]}"`);
export const exactArgs =
  (n: number) =>
  (args: string[]): string | null =>
    args.length === n ? null : `accepts ${n} arg(s), received ${args.length}`;
export const minArgs =
  (n: number) =>
  (args: string[]): string | null =>
    args.length >= n ? null : `requires at least ${n} arg(s), only received ${args.length}`;
export const maxArgs =
  (n: number) =>
  (args: string[]): string | null =>
    args.length <= n ? null : `accepts at most ${n} arg(s), received ${args.length}`;
export const rangeArgs =
  (min: number, max: number) =>
  (args: string[]): string | null =>
    args.length >= min && args.length <= max
      ? null
      : `accepts between ${min} and ${max} arg(s), received ${args.length}`;

export const commandName = (c: Command): string => c.use.split(" ")[0] ?? "";

export interface GroupSpec {
  id: string;
  title: string;
}

const HELP_FLAG: FlagSpec = { name: "help", short: "h", type: "boolean", usage: "help for" };

function flagLine(f: FlagSpec): { left: string; usage: string } {
  const typeName: Record<FlagType, string> = {
    string: " string",
    boolean: "",
    int: " int",
    strings: " strings",
    stringArray: " stringArray",
  };
  let left = "  " + (f.short ? `-${f.short}, ` : "    ") + `--${f.name}` + typeName[f.type];
  left = left.trimEnd();
  let usage = f.usage;
  if (f.default !== undefined && f.default !== "" && f.default !== false && !(f.type === "int" && f.default === 0)) {
    usage +=
      f.type === "string" || f.type === "strings" || f.type === "stringArray"
        ? ` (default "${String(f.default)}")`
        : ` (default ${String(f.default)})`;
  }
  return { left, usage };
}

/** Formats flags like pflag: names left, usage in one aligned column. */
export function formatFlags(flags: FlagSpec[]): string {
  const lines = flags.map(flagLine);
  const width = Math.max(0, ...lines.map((l) => l.left.length));
  return lines.map((l) => `${l.left.padEnd(width)}   ${l.usage}`).join("\n");
}

export interface AppOptions {
  /** Rewrites an error of a command before it is printed, for example to add details. */
  mapError?: (err: unknown) => unknown;
}

export class App {
  readonly root: Command;
  readonly groups: GroupSpec[];
  readonly rootName: string;
  readonly #mapError: (err: unknown) => unknown;
  constructor(root: Command, groups: GroupSpec[] = [], options: AppOptions = {}) {
    this.root = root;
    this.groups = groups;
    this.rootName = commandName(root);
    this.#mapError = options.mapError ?? ((err) => err);
  }

  /** The path of commands from the root to the target, and the arguments left. */
  resolve(argv: string[]): { path: Command[]; rest: string[]; forwarded: boolean } {
    const path: Command[] = [this.root];
    const rest: string[] = [];
    let sawArg = false;
    for (let i = 0; i < argv.length; i++) {
      const cur = path[path.length - 1]!;
      const tok = argv[i] ?? "";
      const sub = sawArg ? undefined : cur.subcommands?.find((s) => commandName(s) === tok || s.aliases?.includes(tok));
      if (!sawArg && cur.forward && !sub && tok !== "-h" && tok !== "--help") {
        return { path, rest: argv.slice(i), forwarded: true };
      }
      if (tok === "--") {
        rest.push(...argv.slice(i));
        break;
      }
      if (tok.startsWith("-") && tok !== "-") {
        rest.push(tok);
        const spec = findFlag(tok, allFlags(path));
        const attached = tok.includes("=") || (!tok.startsWith("--") && tok.length > 2);
        if (spec && spec.type !== "boolean" && !attached && i + 1 < argv.length) {
          i++;
          rest.push(argv[i] ?? "");
        }
        continue;
      }
      if (sub) {
        path.push(sub);
        continue;
      }
      sawArg = true;
      rest.push(tok);
    }
    return { path, rest, forwarded: false };
  }

  help(path: Command[]): string {
    const cmd = path[path.length - 1]!;
    const full = path.map(commandName).join(" ");
    const isRoot = path.length === 1;
    let out = "";
    const desc = cmd.long ?? cmd.short;
    if (desc) out += desc.trimEnd() + "\n\n";
    const subs = (cmd.subcommands ?? [])
      .filter((s) => !s.hidden)
      .sort((a, b) => commandName(a).localeCompare(commandName(b)));
    const runnable = cmd.run !== undefined || cmd.forward !== undefined;
    out += "Usage:\n";
    if (runnable)
      out += `  ${path.slice(0, -1).map(commandName).concat(cmd.use).join(" ")}${hasFlags(path) ? " [flags]" : ""}\n`;
    if (subs.length > 0) out += `  ${full} [command]\n`;
    if (cmd.aliases && cmd.aliases.length > 0)
      out += `\nAliases:\n  ${[commandName(cmd), ...cmd.aliases].join(", ")}\n`;
    if (cmd.example) out += `\nExamples:\n${cmd.example}\n`;
    if (subs.length > 0) {
      const width = Math.max(...subs.map((s) => commandName(s).length));
      const row = (s: Command): string => `  ${commandName(s).padEnd(width)}  ${s.short}`;
      if (isRoot && this.groups.length > 0) {
        for (const g of this.groups) {
          const members = subs.filter((s) => s.group === g.id);
          if (members.length > 0) out += `\n${g.title}\n${members.map(row).join("\n")}\n`;
        }
        const others = [
          ...subs.filter((s) => !s.group),
          { use: "help", short: "Help about any command" } as Command,
        ].sort((a, b) => commandName(a).localeCompare(commandName(b)));
        out += `\nAdditional Commands:\n${others.map(row).join("\n")}\n`;
      } else {
        out += `\nAvailable Commands:\n${subs.map(row).join("\n")}\n`;
      }
    }
    const own: FlagSpec[] = [...(cmd.flags ?? []), ...(cmd.persistentFlags ?? [])];
    const inheritedFlags = path.slice(0, -1).flatMap((c) => c.persistentFlags ?? []);
    const help = { ...HELP_FLAG, usage: `help for ${commandName(cmd)}` };
    const sorted = [...own, help].sort((a, b) => a.name.localeCompare(b.name));
    out += `\nFlags:\n${formatFlags(sorted)}\n`;
    if (inheritedFlags.length > 0) {
      out += `\nGlobal Flags:\n${formatFlags([...inheritedFlags].sort((a, b) => a.name.localeCompare(b.name)))}\n`;
    }
    if (subs.length > 0) out += `\nUse "${full} [command] --help" for more information about a command.\n`;
    return out;
  }

  /** Runs the CLI. Returns the process exit code. */
  async execute(argv: string[], env: Env, globals: Globals): Promise<number> {
    try {
      await this.#execute(argv, env, globals);
      return 0;
    } catch (thrown) {
      const err = this.#mapError(thrown);
      if (err instanceof CliError) {
        if (!err.silent) env.stderr.write(`error: ${err.message}\n`);
        return err.exitCode;
      }
      if (err instanceof Error && err.name === "AbortError") return 1;
      env.stderr.write(`error: ${describeError(err)}\n`);
      return 1;
    }
  }

  async #execute(argv: string[], env: Env, globals: Globals): Promise<void> {
    const { path, rest, forwarded } = this.resolve(argv);
    const cmd = path[path.length - 1]!;
    if (forwarded) {
      await cmd.forward!(rest, env);
      return;
    }
    const stop = rest.indexOf("--");
    const flagArgs = stop < 0 ? rest : rest.slice(0, stop);
    if (flagArgs.includes("-h") || flagArgs.includes("--help")) {
      env.stdout.write(this.help(path));
      return;
    }
    if (cmd === this.root && rest[0] === "help") {
      env.stdout.write(this.help(this.resolve(rest.slice(1)).path));
      return;
    }
    if (cmd.forward) {
      await cmd.forward(rest, env);
      return;
    }
    const specs = allFlags(path);
    const values = new Map<string, unknown>();
    const changed = new Set<string>();
    for (const f of specs)
      values.set(
        f.name,
        f.type === "strings" || f.type === "stringArray"
          ? []
          : f.type === "boolean"
            ? false
            : f.type === "int"
              ? Number(f.default ?? 0)
              : String(f.default ?? ""),
      );
    const options: Record<string, { type: "string" | "boolean"; short?: string; multiple?: boolean }> = {};
    for (const f of specs) {
      const o: { type: "string" | "boolean"; short?: string; multiple?: boolean } = {
        type: f.type === "boolean" ? "boolean" : "string",
      };
      if (f.short) o.short = f.short;
      if (f.type === "strings" || f.type === "stringArray") o.multiple = true;
      options[f.name] = o;
    }
    let parsed;
    try {
      parsed = parseArgs({ args: joinNegatives(rest, specs), options, allowPositionals: true, strict: true });
    } catch (err) {
      throw new CliError(flagError(err));
    }
    for (const f of specs) {
      const raw = parsed.values[f.name];
      if (raw === undefined) continue;
      changed.add(f.name);
      switch (f.type) {
        case "boolean":
          values.set(f.name, raw === true);
          break;
        case "string":
          values.set(f.name, String(raw));
          break;
        case "int": {
          const s = String(raw);
          if (!/^[+-]?\d+$/.test(s))
            throw new CliError(`invalid argument "${s}" for "--${f.name}" flag: not an integer`);
          values.set(f.name, Number(s));
          break;
        }
        case "stringArray":
          values.set(f.name, (raw as string[]).slice());
          break;
        case "strings":
          values.set(
            f.name,
            (raw as string[]).flatMap((v) => v.split(",")),
          );
          break;
      }
    }
    // Environment values apply after the flags are defined, so help never shows them.
    const g: Globals = { ...globals };
    if (changed.has("server")) g.server = String(values.get("server"));
    if (changed.has("api-key")) g.apiKey = String(values.get("api-key"));
    if (changed.has("output")) g.output = String(values.get("output"));
    const args = parsed.positionals;
    if (!cmd.run) {
      if ((cmd.subcommands ?? []).length > 0) {
        if (args.length > 0)
          throw new CliError(`unknown command "${args[0]}" for "${path.map(commandName).join(" ")}"`);
        env.stdout.write(this.help(path));
        return;
      }
      throw new CliError(`command "${commandName(cmd)}" is not runnable`);
    }
    const validate =
      cmd.args ??
      ((a: string[]): string | null =>
        cmd.subcommands && a.length > 0 ? `unknown command "${a[0]}" for "${path.map(commandName).join(" ")}"` : null);
    const bad = validate(args);
    if (bad)
      throw new CliError(
        bad.startsWith("unknown command") && !bad.includes(" for ")
          ? `${bad} for "${path.map(commandName).join(" ")}"`
          : bad,
      );
    await cmd.run({ args, flags: new Flags(values, changed), globals: g, env });
  }
}

function hasFlags(path: Command[]): boolean {
  const cmd = path[path.length - 1]!;
  return (cmd.flags ?? []).length > 0 || (cmd.persistentFlags ?? []).length > 0;
}

function allFlags(path: Command[]): FlagSpec[] {
  const out: FlagSpec[] = [];
  for (const c of path) out.push(...(c.persistentFlags ?? []));
  out.push(...(path[path.length - 1]!.flags ?? []));
  out.push({ name: "help", short: "h", type: "boolean", usage: "help" });
  return out;
}

function findFlag(tok: string, specs: FlagSpec[]): FlagSpec | undefined {
  if (tok.startsWith("--")) {
    const name = tok.slice(2).split("=")[0];
    return specs.find((f) => f.name === name);
  }
  return specs.find((f) => f.short === tok.slice(1, 2));
}

/** Joins "--flag -1" into "--flag=-1" so that a negative number is a value. */
function joinNegatives(args: string[], specs: FlagSpec[]): string[] {
  const out: string[] = [];
  for (let i = 0; i < args.length; i++) {
    const a = args[i] ?? "";
    const next = args[i + 1];
    const spec = a.startsWith("--") && !a.includes("=") ? findFlag(a, specs) : undefined;
    if (spec && spec.type !== "boolean" && next !== undefined && /^-\d/.test(next)) {
      out.push(`${a}=${next}`);
      i++;
    } else if (a === "--") {
      out.push(...args.slice(i));
      break;
    } else out.push(a);
  }
  return out;
}

function flagError(err: unknown): string {
  const msg = err instanceof Error ? err.message : String(err);
  const unknown = /Unknown option '([^']+)'/.exec(msg);
  if (unknown) return `unknown flag: ${unknown[1]}`;
  const missing = /Option '([^']+)' argument missing/.exec(msg);
  if (missing) return `flag needs an argument: ${missing[1]?.split(",").pop()?.trim().split(" ")[0]}`;
  const ambiguous = /Option '([^']+)' argument is ambiguous/.exec(msg);
  if (ambiguous) return `flag needs an argument: ${ambiguous[1]?.split(",").pop()?.trim().split(" ")[0]}`;
  return msg;
}

/** The message of an error, with the cause of a failed fetch (for example ECONNREFUSED). */
function describeError(err: unknown): string {
  if (!(err instanceof Error)) return String(err);
  const cause = err.cause;
  if (cause instanceof Error && cause.message !== "" && !err.message.includes(cause.message))
    return `${err.message}: ${cause.message}`;
  return err.message;
}
