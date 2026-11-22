import { createApp, globalsFrom } from "../src/cli.ts";
import type { Env } from "../src/framework.ts";

export interface Call {
  method: string;
  url: string;
  headers: Headers;
  body: string;
}

export type Handler = (call: Call) => Response | Promise<Response>;

export const json = (v: unknown, status = 200): Response =>
  new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json" } });

export const problem = (status: number, code: string, title: string, extra: Record<string, unknown> = {}): Response =>
  json({ code, title, status, ...extra }, status);

export interface CliResult {
  code: number;
  out: string;
  err: string;
  calls: Call[];
  ran: { file: string; args: string[] }[];
}

export interface CliOptions {
  handler?: Handler;
  vars?: Record<string, string | undefined>;
  stdin?: string;
  cwd?: string;
  programExit?: number;
  now?: Date;
}

/** Runs the CLI in process with a fake fetch and fake process. */
export async function runCli(args: string[], o: CliOptions = {}): Promise<CliResult> {
  const calls: Call[] = [];
  const ran: { file: string; args: string[] }[] = [];
  let out = "";
  let err = "";
  const vars = { PATH: "", ...(o.vars ?? {}) };
  const stdin = o.stdin ?? "";
  const env: Env = {
    stdout: { write: (c) => void (out += c) },
    stderr: { write: (c) => void (err += c) },
    readStdin: () => Promise.resolve(stdin),
    stdin: (async function* () {
      if (stdin !== "") yield stdin;
    })(),
    vars,
    cwd: o.cwd ?? process.cwd(),
    signal: new AbortController().signal,
    now: () => o.now ?? new Date(),
    runProgram: (file, a) => {
      ran.push({ file, args: a });
      return Promise.resolve(o.programExit ?? 0);
    },
    fetch: (async (input: string | URL | Request, init?: RequestInit) => {
      const req = new Request(input, init);
      const call: Call = { method: req.method, url: req.url, headers: req.headers, body: await req.text() };
      calls.push(call);
      if (!o.handler) return json({ code: "internal_error", title: "no handler" }, 500);
      return o.handler(call);
    }) as typeof fetch,
  };
  const code = await createApp().execute(args, env, globalsFrom(vars));
  return { code, out, err, calls, ran };
}

/** A handler that answers by "METHOD /path?query" from a table. */
export function router(routes: Record<string, Response | (() => Response) | ((c: Call) => Response)>): Handler {
  return (call) => {
    const u = new URL(call.url);
    const key = `${call.method} ${u.pathname.replace(/^\/api\/v1/, "")}`;
    const r = routes[key] ?? routes[`${key}${u.search}`];
    if (!r) return problem(404, "resource_not_found", `no route ${key}`);
    return typeof r === "function" ? (r as (c: Call) => Response)(call) : r.clone();
  };
}
