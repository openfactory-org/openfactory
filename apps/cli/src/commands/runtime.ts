// Commands that belong to the Go runtime binary (openfactory-runtime): they
// run inside sandboxes, on worker hosts and in microVMs. The CLI forwards
// them when the binary is on PATH.

import { accessSync, constants, statSync } from "node:fs";
import { delimiter, join } from "node:path";
import { CliError, type Command, type Env } from "../framework.ts";

export const RUNTIME_BINARY = "openfactory-runtime";

/** Finds an executable on PATH (and PATHEXT on Windows). */
export function findOnPath(name: string, vars: Record<string, string | undefined>): string | null {
  const dirs = (vars["PATH"] ?? "").split(delimiter).filter(Boolean);
  const exts = process.platform === "win32" ? ["", ...(vars["PATHEXT"] ?? ".EXE;.CMD").split(";")] : [""];
  for (const d of dirs) {
    for (const ext of exts) {
      const p = join(d, name + ext);
      try {
        if (!statSync(p).isFile()) continue;
        accessSync(p, constants.X_OK);
        return p;
      } catch {
        // not here
      }
    }
  }
  return null;
}

/** Runs `openfactory-runtime <command> <args...>` and ends the process with its exit code. */
export async function forwardToRuntime(command: string, args: string[], env: Env): Promise<void> {
  const bin = findOnPath(RUNTIME_BINARY, env.vars);
  if (bin === null) {
    throw new CliError(
      `"openfactory ${command}" needs ${RUNTIME_BINARY}, which is not on PATH. ` +
        `${RUNTIME_BINARY} runs the in-sandbox runner, the egress proxy (netguard), remote workers and the microVM agent. ` +
        "Install it from the release archive or the Open Factory images, then try again.",
    );
  }
  const code = await env.runProgram(bin, [command, ...args]);
  if (code !== 0) throw new CliError("", true, code);
}

const forwarded = (use: string, short: string): Command => ({
  use,
  short,
  group: "run",
  forward: (args, env) => forwardToRuntime(use.split(" ")[0] ?? use, args, env),
  long: `${short}.\n\nThis command is part of ${RUNTIME_BINARY}. The CLI passes all arguments to "${RUNTIME_BINARY} ${use.split(" ")[0] ?? use}".\nRun "${RUNTIME_BINARY} ${use.split(" ")[0] ?? use} --help" for its flags.`,
});

export const runnerCommand = forwarded("runner", "Run one session of a run inside a sandbox");
export const netguardCommand = forwarded("netguard", "Run the egress proxy that enforces a network policy");
export const vmInitCommand = forwarded("vm-init", "Run the init process of a microVM worker");

export const serveCommand: Command = {
  use: "serve",
  short: "Removed: run the control plane with wrangler",
  hidden: true,
  run() {
    throw new CliError(
      'the "serve" command was removed. The control plane is a Cloudflare Worker: run it locally with `npm run dev` (wrangler dev) and deploy it with `wrangler deploy`.',
    );
  },
};
