// Session sources: how a test gets the task variables of a run session so that
// it can act as the runner.
//
//   shim   (go target)     The Go server's local sandbox backend starts
//                          tools/contract/runner-shim.mjs, which writes the
//                          task variables to the rendezvous directory.
//   worker (worker target) The local provider sends task.assign to a fake
//                          worker that the suite enrolled and connected.
import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, readFileSync, writeFileSync, renameSync } from "node:fs";
import { join } from "node:path";
import { afterAll, beforeAll } from "vitest";
import { type Api, uniq, waitFor } from "./api.ts";
import { adminApi } from "./context.ts";
import { REPO_ROOT } from "./paths.ts";
import type { TaskEnv } from "./runner.ts";
import { target } from "./target.ts";
import { FakeWorker } from "./worker.ts";

export interface SessionSource {
  readonly kind: "shim" | "worker";
  /** Waits until the target starts session `session` of the run and returns its task variables. */
  awaitSession(runId: string, session?: number, timeoutMs?: number): Promise<TaskEnv>;
  /** Whether the target started the session (without waiting). */
  hasSession(runId: string, session?: number): boolean;
  /** Ends the sandbox of the session (the fake runner process exits). */
  release(runId: string, session?: number): void;
  /** Runs a real runner command in the sandbox of the session, with the task variables. */
  exec(runId: string, session: number, command: string[], env?: Record<string, string>): void;
  close(): Promise<void>;
}

class ShimSessions implements SessionSource {
  readonly kind = "shim" as const;
  constructor(private readonly dir: string) {
    if (!dir) throw new Error("OF_CONTRACT_RENDEZVOUS_DIR is not set (go target)");
  }
  private base(runId: string, session: number): string {
    return join(this.dir, `${runId}.${session}`);
  }
  hasSession(runId: string, session = 1): boolean {
    return existsSync(`${this.base(runId, session)}.json`);
  }
  async awaitSession(runId: string, session = 1, timeoutMs = 30_000): Promise<TaskEnv> {
    const file = `${this.base(runId, session)}.json`;
    return waitFor(
      `session ${session} of run ${runId} (${file})`,
      async () => {
        if (!existsSync(file)) return undefined;
        const rec = JSON.parse(readFileSync(file, "utf8")) as { env: TaskEnv };
        return rec.env;
      },
      timeoutMs,
      100,
    );
  }
  release(runId: string, session = 1): void {
    writeFileSync(`${this.base(runId, session)}.exit`, "");
  }
  exec(runId: string, session: number, command: string[], env: Record<string, string> = {}): void {
    const base = this.base(runId, session);
    writeFileSync(`${base}.exec.json.tmp`, JSON.stringify({ command, env, cwd: REPO_ROOT }));
    renameSync(`${base}.exec.json.tmp`, `${base}.exec.json`);
  }
  async close(): Promise<void> {}
}

class WorkerSessions implements SessionSource {
  readonly kind = "worker" as const;
  private worker: FakeWorker | undefined;
  private connecting: Promise<FakeWorker> | undefined;
  private readonly children: ChildProcess[] = [];
  workerId = "";

  constructor(private readonly api: Api) {}

  /** Enrolls and connects the fake worker (once). */
  connect(): Promise<FakeWorker> {
    this.connecting ??= (async () => {
      // The worker target runs with OF_SANDBOX_PROVIDER=local, which places
      // tasks on development process workers: a worker that connects with
      // the admin API key (OF_DEV_PROCESS_WORKERS=true on the control plane).
      const name = uniq("ct-worker");
      this.workerId = name;
      this.worker = await FakeWorker.connect(
        { kind: "bearer", token: this.api.key ?? "" },
        { workerId: name, name, backend: "process", capacity: 8, slots: 8 },
      );
      return this.worker;
    })();
    return this.connecting;
  }

  hasSession(runId: string, session = 1): boolean {
    if (!this.worker) return false;
    for (const t of this.worker.tasks.values()) if (t.run_id === runId && t.session === session) return true;
    return false;
  }
  async awaitSession(runId: string, session = 1, timeoutMs = 30_000): Promise<TaskEnv> {
    const w = await this.connect();
    const task = await w.awaitTask(runId, session, timeoutMs);
    return task.env;
  }
  release(runId: string, session = 1): void {
    const w = this.worker;
    if (!w) return;
    for (const t of w.tasks.values()) {
      if (t.run_id === runId && t.session === session) {
        w.taskState({
          task_id: t.task_id,
          state: "exited",
          reason: "completed",
          exit_code: 0,
          ended_at: new Date().toISOString(),
        });
      }
    }
  }
  exec(runId: string, session: number, command: string[], env: Record<string, string> = {}): void {
    const w = this.worker;
    const task = w && [...w.tasks.values()].find((t) => t.run_id === runId && t.session === session);
    if (!task) throw new Error(`no task for ${runId}.${session}`);
    const [cmd, ...args] = command;
    if (!cmd) throw new Error("empty command");
    const child = spawn(cmd, args, { cwd: REPO_ROOT, env: { ...process.env, ...task.env, ...env }, stdio: "inherit" });
    this.children.push(child);
  }
  async close(): Promise<void> {
    for (const c of this.children) if (c.exitCode === null) c.kill("SIGTERM");
    this.worker?.close();
  }
}

/** The session source of the target. Closed after the test file. */
export function useSessions(): SessionSource {
  let src: SessionSource | undefined;
  const get = (): SessionSource => {
    if (!src) {
      const kind = process.env.OF_CONTRACT_SESSION_SOURCE ?? (target.name === "go" ? "shim" : "worker");
      src = kind === "shim" ? new ShimSessions(target.rendezvousDir) : new WorkerSessions(adminApi());
    }
    return src;
  };
  // The worker source connects before the first test creates a run: a run
  // created while no worker is connected would start at a later session.
  beforeAll(async () => {
    const s = get();
    if (s instanceof WorkerSessions) await s.connect();
  });
  afterAll(async () => {
    await src?.close();
  });
  // A proxy, so that the source is created lazily inside a test (after inject() works).
  return {
    get kind() {
      return get().kind;
    },
    awaitSession: (r, s, t) => get().awaitSession(r, s, t),
    hasSession: (r, s) => get().hasSession(r, s),
    release: (r, s) => get().release(r, s),
    exec: (r, s, c, e) => get().exec(r, s, c, e),
    close: () => get().close(),
  };
}
