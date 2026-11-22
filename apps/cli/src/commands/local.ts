import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { basename, dirname, join, resolve } from "node:path";
import {
  compile,
  defaultDefinition,
  SCHEMA_BASE_URL,
  SCHEMA_VERSION,
  schemas,
  type ValidationError,
} from "@openfactory/definition";
import { loadDir } from "@openfactory/definition/node";
import { validateSchema } from "@openfactory/definition/validate";
import { emit } from "../format.ts";
import { CliError, maxArgs, type Command } from "../framework.ts";
import { parseRepoSpec } from "../repo.ts";

/** Formats an error as dir/file:line: field: message. */
export function formatValidationError(dir: string, e: ValidationError): string {
  let loc = join(dir, e.file);
  if (e.line !== undefined && e.line > 0) loc += `:${e.line}`;
  let msg = e.message;
  if (e.field && !msg.includes(e.field)) msg = `${e.field}: ${msg}`;
  return `${loc}: ${msg}`;
}

export const validateCommand: Command = {
  use: "validate [dir...]",
  short: "Validate a factory definition tree (default: current directory)",
  long: `Load the definition tree rooted at dir (the directory holding factory.yaml),
validate it against the v1 schemas and semantic rules, and compile it.
Errors are printed as file:line: message; the exit status is 1 when the
definition is invalid. Several directories are checked one after another.`,
  async run({ args, env }) {
    const dirs = args.length === 0 ? ["."] : args;
    let failed = false;
    for (const dir of dirs) {
      const files = loadDir(resolve(env.cwd, dir));
      // A glob such as examples/*/ also matches directories that are not factories.
      if (dirs.length > 1 && files["factory.yaml"] === undefined && files["factory.yml"] === undefined) {
        env.stdout.write(`${dir}: skipped (no factory.yaml)\n`);
        continue;
      }
      const { definition: def, errors } = compile(files);
      const all: string[] = errors.map((e) => formatValidationError(dir, e));
      if (def !== null && errors.length === 0) {
        for (const [path, content] of Object.entries(files)) {
          const r = validateSchema(path, content);
          for (const se of r.errors)
            all.push(`${join(dir, path)}: schema ${se.instance || "/"}: ${se.message} (${se.keyword})`);
        }
      }
      if (all.length > 0 || def === null) {
        for (const line of all) env.stdout.write(line + "\n");
        env.stderr.write(`${dir}: ${all.length} error(s)\n`);
        failed = true;
        continue;
      }
      env.stdout.write(
        `${dir}: valid (${def.name}) — ${def.agents?.length ?? 0} agent(s), ${def.automations?.length ?? 0} automation(s), ${def.webhooks?.length ?? 0} webhook(s), ${def.scorers?.length ?? 0} scorer(s), ${def.benchmarks?.length ?? 0} benchmark(s)\n`,
      );
    }
    if (failed) throw new CliError("", true);
  },
};

export const schemaCommand: Command = {
  use: "schema [document]",
  short: "List the definition JSON Schemas or print one",
  long: `Without arguments, list the JSON Schema documents of the current definition
schema version. With a document name (e.g. factory.schema.json, or just
"factory"), print that schema.`,
  args: maxArgs(1),
  async run({ args, env }) {
    const docs = schemas();
    if (args.length === 0) {
      env.stdout.write(`Schema version ${SCHEMA_VERSION} (${SCHEMA_BASE_URL})\n`);
      for (const n of Object.keys(docs).sort()) env.stdout.write(`  ${n}\n`);
      return;
    }
    const name = args[0] ?? "";
    const doc = docs[name] ?? docs[name + ".schema.json"];
    if (doc === undefined)
      throw new CliError(`unknown schema document ${JSON.stringify(name)} (run \`openfactory schema\` to list them)`);
    try {
      emit(env.stdout, JSON.parse(doc));
    } catch {
      env.stdout.write(doc);
    }
  },
};

export const initCommand: Command = {
  use: "init [dir]",
  short: "Scaffold a factory definition with the default agents",
  long: `Write a default definition tree (factory.yaml plus the foreman and the
default stage agents) into dir (default: current directory).

Example:
  openfactory init my-factory --name "Payments" --repo acme/payments --repo gitlab:acme/web`,
  args: maxArgs(1),
  flags: [
    {
      name: "agents",
      type: "strings",
      usage: "default stage agents to include (triage,spec,implement,review); default all",
    },
    { name: "alias", type: "string", usage: "foreman @alias" },
    { name: "force", type: "boolean", usage: "overwrite an existing definition" },
    { name: "model", type: "string", usage: "default model for all agents" },
    { name: "name", type: "string", usage: "factory name (default: directory name)" },
    {
      name: "repo",
      type: "stringArray",
      usage: "repository [forge:]owner/name (repeatable; forge github, gitlab or azure_devops)",
    },
  ],
  async run({ args, flags, env }) {
    const dir = args[0] ?? ".";
    const abs = resolve(env.cwd, dir);
    const name = flags.string("name") || basename(abs);
    const repositories = flags.list("repo").map(parseRepoSpec);
    let files: Record<string, string>;
    try {
      files = defaultDefinition({
        name,
        alias: flags.string("alias"),
        repositories,
        model: flags.string("model"),
        agents: flags.list("agents"),
      });
    } catch (err) {
      throw new CliError(err instanceof Error ? err.message : String(err));
    }
    if (!flags.bool("force")) {
      for (const p of ["factory.yaml", "factory.yml"]) {
        if (existsSync(join(abs, p))) throw new CliError(`${dir} already contains ${p} (use --force to overwrite)`);
      }
    }
    for (const p of Object.keys(files).sort()) {
      const full = join(abs, p);
      mkdirSync(dirname(full), { recursive: true });
      writeFileSync(full, files[p] ?? "", { mode: 0o644 });
      env.stdout.write(`created ${join(dir, p)}\n`);
    }
    const { errors } = compile(files);
    if (errors.length > 0) throw new CliError("scaffolded definition is invalid: " + (errors[0]?.message ?? ""));
    env.stdout.write(
      `\nNext: edit the agents, then \`openfactory validate ${dir}\` and \`openfactory factory apply ${dir}\`.\n`,
    );
  },
};
