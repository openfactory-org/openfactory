// The cloudflare-sandbox provider Worker. Its own deploy: a
// control-plane deploy does not restart sandboxes.
//
// Exports:
// - SandboxProvider: WorkerEntrypoint with the SandboxProvider RPC methods. The
//   control plane binds it as PROVIDER_CLOUDFLARE_SANDBOX.
// - OfTaskSandbox: one Durable Object per task, with ctx.container.
// - OfTaskRegistry: the task index for list() (no platform list API).
// - Egress: the policy proxy of one task, behind the outbound intercepts.

import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";
import { blockedMessage, decideEgress, type EgressProps } from "./egress";
import { CloudflareSandboxProvider, PROVIDER_ID, type ProviderDeps, type TaskSandboxStub } from "./provider";
import { Registry, type ListableKv, type RegistryPage } from "./registry";
import type {
  InstanceType,
  PreflightResult,
  ProviderCapabilities,
  SandboxTask,
  TaskStatus,
} from "@openfactory/sandbox";
import { TaskSandbox, type ContainerLike } from "./task-sandbox";

export { PROVIDER_ID };

export interface Env {
  SANDBOXES: DurableObjectNamespace<OfTaskSandbox>;
  REGISTRY: DurableObjectNamespace<OfTaskRegistry>;
  /** Service binding to the control-plane Worker. */
  CONTROL_PLANE: Fetcher;
  /** Install ID; keys the registry. */
  OF_INSTALL_ID: string;
  OF_RUNNER_VERSION: string;
  /** Image key in containers[].images. Default "runner". */
  OF_IMAGE_KEY?: string;
  /** "eu" keeps sandboxes in the EU (DO jurisdiction). Empty: no restriction. */
  OF_JURISDICTION?: string;
}

function sandboxes(env: Env): DurableObjectNamespace<OfTaskSandbox> {
  return env.OF_JURISDICTION === "eu" ? env.SANDBOXES.jurisdiction("eu") : env.SANDBOXES;
}

function sandboxStub(env: Env, taskId: string): DurableObjectStub<OfTaskSandbox> {
  const ns = sandboxes(env);
  return ns.get(ns.idFromName(taskId));
}

function registryStub(env: Env): DurableObjectStub<OfTaskRegistry> {
  return env.REGISTRY.get(env.REGISTRY.idFromName(env.OF_INSTALL_ID || "default"));
}

export class OfTaskSandbox extends DurableObject<Env> {
  private readonly core: TaskSandbox;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    if (!ctx.container) throw new Error("OfTaskSandbox needs a container binding (containers[] in wrangler.jsonc)");
    const exportsWithEgress = ctx.exports as unknown as { Egress(o: { props: EgressProps }): Fetcher };
    this.core = new TaskSandbox({
      container: ctx.container as unknown as ContainerLike,
      kv: ctx.storage.kv,
      setAlarm: (at) => void ctx.storage.setAlarm(at),
      deleteAlarm: () => void ctx.storage.deleteAlarm(),
      now: () => Date.now(),
      egress: (props) => exportsWithEgress.Egress({ props }),
      registry: {
        add: (id) => registryStub(env).add(id),
        remove: (id) => registryStub(env).remove(id),
      },
      waitUntil: (p) => ctx.waitUntil(p),
      imageKey: env.OF_IMAGE_KEY || "runner",
    });
    void ctx.blockConcurrencyWhile(() => this.core.onStartup());
  }

  start(task: SandboxTask, instance: InstanceType): Promise<TaskStatus> {
    return this.core.start(task, instance);
  }
  status(): TaskStatus {
    return this.core.status();
  }
  cancel(graceSeconds: number): Promise<void> {
    return this.core.cancel(graceSeconds);
  }
  destroy(): Promise<void> {
    return this.core.destroy();
  }
  ack(): void {
    this.core.ack();
  }
  images(): string[] {
    return this.core.images();
  }
  recordBlocked(host: string): void {
    this.core.recordBlocked(host);
  }
  blockedReport(): { blocked: ReturnType<TaskSandbox["blocked"]> } {
    return { blocked: this.core.blocked() };
  }
  override async alarm(): Promise<void> {
    await this.core.alarm();
  }
}

export class OfTaskRegistry extends DurableObject<Env> {
  private readonly reg: Registry;
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.reg = new Registry(ctx.storage.kv as unknown as ListableKv);
  }
  add(taskId: string): void {
    this.reg.add(taskId, Date.now());
  }
  remove(taskId: string): void {
    this.reg.remove(taskId);
  }
  list(cursor?: string): RegistryPage {
    return this.reg.list(cursor);
  }
}

/** The policy proxy of one task. Props come from ctx.exports.Egress({props}). */
export class Egress extends WorkerEntrypoint<Env, EgressProps> {
  override async fetch(req: Request): Promise<Response> {
    const props = this.ctx.props;
    const d = decideEgress(props, req.url);
    switch (d.kind) {
      case "control":
        return this.env.CONTROL_PLANE.fetch(req);
      case "report":
        return Response.json(await sandboxStub(this.env, props.taskId).blockedReport());
      case "blocked":
        this.ctx.waitUntil(sandboxStub(this.env, props.taskId).recordBlocked(d.host));
        return new Response(blockedMessage(d.host), {
          status: 403,
          headers: { "content-type": "text/plain; charset=utf-8" },
        });
      case "forward":
        return fetch(req);
    }
  }
}

function deps(env: Env): ProviderDeps {
  const d: ProviderDeps = {
    sandbox: (taskId) => sandboxStub(env, taskId) as unknown as TaskSandboxStub,
    registry: () => registryStub(env),
    controlPlane: env.CONTROL_PLANE,
    runnerVersion: env.OF_RUNNER_VERSION,
    imageKey: env.OF_IMAGE_KEY || "runner",
    regions: env.OF_JURISDICTION === "eu" ? ["eu"] : ["auto"],
  };
  return d;
}

/** SandboxProvider over a service binding (SandboxProviderRpc: `id` is a method). */
export class SandboxProvider extends WorkerEntrypoint<Env> {
  private get p(): CloudflareSandboxProvider {
    return new CloudflareSandboxProvider(deps(this.env));
  }
  async id(): Promise<string> {
    return PROVIDER_ID;
  }
  capabilities(): Promise<ProviderCapabilities> {
    return this.p.capabilities();
  }
  preflight(task: SandboxTask): Promise<PreflightResult> {
    return this.p.preflight(task);
  }
  start(task: SandboxTask): Promise<TaskStatus> {
    return this.p.start(task);
  }
  status(taskId: string): Promise<TaskStatus> {
    return this.p.status(taskId);
  }
  cancel(taskId: string, graceSeconds: number): Promise<void> {
    return this.p.cancel(taskId, graceSeconds);
  }
  destroy(taskId: string): Promise<void> {
    return this.p.destroy(taskId);
  }
  list(): Promise<TaskStatus[]> {
    return this.p.list();
  }
  /** Not part of SandboxProvider: the control plane calls it after session.started. */
  ack(taskId: string): Promise<void> {
    return this.p.ack(taskId);
  }
}

export default {
  fetch(): Response {
    return new Response("not found", { status: 404 });
  },
} satisfies ExportedHandler<Env>;
