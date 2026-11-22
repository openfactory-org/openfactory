// The openfactory CLI entry point.

import { spawn } from "node:child_process";
import { main } from "./cli.ts";
import type { Env } from "./framework.ts";

function nodeEnv(signal: AbortSignal): Env {
  return {
    stdout: { write: (c) => void process.stdout.write(c) },
    stderr: { write: (c) => void process.stderr.write(c) },
    stdin: process.stdin,
    readStdin: async () => {
      let out = "";
      for await (const chunk of process.stdin)
        out += typeof chunk === "string" ? chunk : (chunk as Buffer).toString("utf8");
      return out;
    },
    vars: process.env,
    cwd: process.cwd(),
    fetch: globalThis.fetch,
    signal,
    runProgram: (file, args) =>
      new Promise((resolve, reject) => {
        const child = spawn(file, args, { stdio: "inherit" });
        const forward = (sig: NodeJS.Signals) => (): void => void child.kill(sig);
        const onInt = forward("SIGINT");
        const onTerm = forward("SIGTERM");
        process.on("SIGINT", onInt);
        process.on("SIGTERM", onTerm);
        child.on("error", reject);
        child.on("exit", (code, sig) => {
          process.off("SIGINT", onInt);
          process.off("SIGTERM", onTerm);
          resolve(code ?? (sig ? 128 : 1));
        });
      }),
    now: () => new Date(),
  };
}

const ac = new AbortController();
const stop = (): void => ac.abort();
process.once("SIGINT", stop);
process.once("SIGTERM", stop);
// A closed pipe (for example `openfactory run logs x | head`) ends the command.
process.stdout.on("error", (err: NodeJS.ErrnoException) => {
  if (err.code === "EPIPE") process.exit(0);
});

const code = await main(process.argv.slice(2), nodeEnv(ac.signal));
process.exitCode = code;
// Open handles (a stdio bridge or a keep-alive socket) must not hold the process.
process.exit(code);
