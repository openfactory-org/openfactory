// Fakes for ctx.container, ctx.storage.kv and the DO namespaces.
import { Registry, type ListableKv } from "../src/registry";
import { CloudflareSandboxProvider, type TaskSandboxStub } from "../src/provider";
import { TaskSandbox, type ContainerLike } from "../src/task-sandbox";
import type { EgressProps } from "../src/egress";

export class FakeKv implements ListableKv {
  readonly m = new Map<string, unknown>();
  get<T>(k: string): T | undefined {
    const v = this.m.get(k);
    return v === undefined ? undefined : (structuredClone(v) as T);
  }
  put<T>(k: string, v: T): void {
    this.m.set(k, structuredClone(v));
  }
  delete(k: string): boolean {
    return this.m.delete(k);
  }
  *list<T>(o: { prefix?: string; startAfter?: string; limit?: number } = {}): Iterable<[string, T]> {
    let n = 0;
    for (const k of [...this.m.keys()].sort()) {
      if (o.prefix && !k.startsWith(o.prefix)) continue;
      if (o.startAfter && k <= o.startAfter) continue;
      if (o.limit !== undefined && n >= o.limit) return;
      n++;
      yield [k, this.m.get(k) as T];
    }
  }
}

export class FakeContainer implements ContainerLike {
  running = false;
  images: Record<string, string> = { runner: "registry.cloudflare.example/of-sandbox@sha256:0000" };
  log: string[] = [];
  started?: Parameters<ContainerLike["start"]>[0];
  intercepts: { kind: string; addr?: string; binding: Fetcher }[] = [];
  startError?: Error;
  private settle?: { resolve: () => void; reject: (e: unknown) => void };

  start(o: Parameters<ContainerLike["start"]>[0]): void {
    this.log.push("start");
    if (this.startError) throw this.startError;
    if (this.running) throw new Error("already running");
    this.started = o;
    this.running = true;
  }
  monitor(): Promise<void> {
    return new Promise((resolve, reject) => (this.settle = { resolve, reject }));
  }
  /** Ends the process: code 0 resolves monitor(), other codes reject it. */
  exit(code: number, message = `container exited with code ${code}`): void {
    this.running = false;
    if (code === 0) this.settle?.resolve();
    else this.settle?.reject(Object.assign(new Error(message), { exitCode: code }));
  }
  /** The platform stops the instance while the DO is not in memory: no outcome. */
  vanish(): void {
    this.running = false;
  }
  async destroy(reason?: unknown): Promise<void> {
    this.log.push(`destroy:${String(reason)}`);
    this.running = false;
  }
  signal(n: number): void {
    this.log.push(`signal:${n}`);
  }
  async setInactivityTimeout(ms: number): Promise<void> {
    this.log.push(`timeout:${ms}`);
  }
  async interceptAllOutboundHttp(binding: Fetcher): Promise<void> {
    this.log.push("intercept:http");
    this.intercepts.push({ kind: "http", binding });
  }
  async interceptOutboundHttps(addr: string, binding: Fetcher): Promise<void> {
    this.log.push(`intercept:https:${addr}`);
    this.intercepts.push({ kind: "https", addr, binding });
  }
}

export interface Harness {
  clock: { now: number };
  alarms: (number | null)[];
  container: FakeContainer;
  kv: FakeKv;
  egressProps: EgressProps[];
  registry: Registry;
  pending: Promise<unknown>[];
  sandbox: TaskSandbox;
  flush(): Promise<void>;
}

export function harness(shared?: { registry: Registry; clock: { now: number } }): Harness {
  const clock = shared?.clock ?? { now: Date.parse("2026-11-22T10:00:00Z") };
  const registry = shared?.registry ?? new Registry(new FakeKv());
  const h: Partial<Harness> = {
    clock,
    alarms: [],
    container: new FakeContainer(),
    kv: new FakeKv(),
    egressProps: [],
    registry,
    pending: [],
  };
  h.sandbox = new TaskSandbox({
    container: h.container!,
    kv: h.kv!,
    setAlarm: (at) => h.alarms!.push(at),
    deleteAlarm: () => h.alarms!.push(null),
    now: () => clock.now,
    egress: (props) => {
      h.egressProps!.push(props);
      return { fetch: async () => new Response("egress") } as unknown as Fetcher;
    },
    registry: { add: async (id) => registry.add(id, clock.now), remove: async (id) => registry.remove(id) },
    waitUntil: (p) => h.pending!.push(p),
  });
  h.flush = async () => {
    await Promise.allSettled(h.pending!);
    await new Promise((r) => setTimeout(r, 0));
  };
  return h as Harness;
}

/** A provider over in-process TaskSandbox instances (one per task ID). */
export function fakeProvider(): {
  provider: CloudflareSandboxProvider;
  sandboxes: Map<string, Harness>;
  registry: Registry;
} {
  const registry = new Registry(new FakeKv());
  const clock = { now: Date.parse("2026-11-22T10:00:00Z") };
  const sandboxes = new Map<string, Harness>();
  const get = (id: string): Harness => {
    let h = sandboxes.get(id);
    if (!h) sandboxes.set(id, (h = harness({ registry, clock })));
    return h;
  };
  const stub = (id: string): TaskSandboxStub => {
    const s = get(id).sandbox;
    return {
      start: (t, i) => s.start(t, i),
      status: async () => s.status(),
      cancel: (g) => s.cancel(g),
      destroy: () => s.destroy(),
      ack: async () => s.ack(),
      images: async () => s.images(),
    };
  };
  const provider = new CloudflareSandboxProvider({
    sandbox: stub,
    registry: () => ({ list: async (c) => registry.list(c), remove: async (id) => registry.remove(id) }),
    runnerVersion: "0.0.0-test",
    controlPlane: { fetch: async () => Response.json({ version: "0.0.0-test" }) } as unknown as Fetcher,
  });
  return { provider, sandboxes, registry };
}
