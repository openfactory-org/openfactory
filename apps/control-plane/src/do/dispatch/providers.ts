// Sandbox target resolution and the provider registry.
//
// workerHost (run config) names a target:
//   "" | "server"     the default provider (OF_SANDBOX_PROVIDER; "local" when unset)
//   "pool:<label>"    a pool of workers with that label
//   a provider ID     cloudflare-sandbox, daytona, boatdev, local, process, microvm or a plugin
//   anything else     a worker ID (WorkerHubDO)
//
// Providers that run in-process register a factory with registerProvider()
// (bootstrapControlPlane in src/wiring.ts). cloudflare-sandbox is the service
// binding. A worker target is the microvm provider of its WorkerHubDO.
import { OfError } from "@openfactory/core";
import type { ProviderCapabilities, SandboxProvider, SandboxTask, TaskStatus } from "@openfactory/sandbox";
import type { DispatchTarget } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { workerProvider } from "../workerhub/providers";

export const DEFAULT_PROVIDER = "local";

export type ProviderFactory = (env: Env, teamId: string) => SandboxProvider;

const factories = new Map<string, ProviderFactory>();

/** Registers an in-process provider (daytona, boatdev, local, or one of a custom build). A second call replaces the first. */
export function registerProvider(id: string, factory: ProviderFactory): void {
  factories.set(id, factory);
}

export function unregisterProvider(id: string): void {
  factories.delete(id);
}

export function registeredProviders(): string[] {
  return [...factories.keys()];
}

const BUILTIN_IDS = new Set(["cloudflare-sandbox", "daytona", "boatdev", "local", "process", "microvm"]);

export function defaultProviderId(env: Pick<Env, "OF_SANDBOX_PROVIDER">): string {
  return (env.OF_SANDBOX_PROVIDER ?? "").trim() || DEFAULT_PROVIDER;
}

/** The target of a workerHost value. */
export function targetOf(env: Pick<Env, "OF_SANDBOX_PROVIDER">, workerHost: string | undefined): DispatchTarget {
  const h = (workerHost ?? "").trim();
  if (h === "" || h === "server") return { kind: "provider", providerId: defaultProviderId(env) };
  if (h.startsWith("pool:")) return { kind: "pool", label: h.slice(5) };
  if (BUILTIN_IDS.has(h) || factories.has(h)) return { kind: "provider", providerId: h };
  return { kind: "worker", workerId: h };
}

/** A stable string key of a target: the FIFO lane of DispatchDO. */
export function targetKey(t: DispatchTarget): string {
  switch (t.kind) {
    case "provider":
      return `provider:${t.providerId}`;
    case "worker":
      return `worker:${t.workerId}`;
    case "pool":
      return `pool:${t.label}`;
  }
}

class ServiceBindingProvider implements SandboxProvider {
  constructor(
    readonly id: string,
    private readonly b: Env["PROVIDER_CLOUDFLARE_SANDBOX"],
  ) {}
  capabilities(): Promise<ProviderCapabilities> {
    return this.b.capabilities();
  }
  preflight(task: SandboxTask) {
    return this.b.preflight(task);
  }
  start(task: SandboxTask): Promise<TaskStatus> {
    return this.b.start(task);
  }
  status(taskId: string): Promise<TaskStatus> {
    return this.b.status(taskId);
  }
  cancel(taskId: string, graceSeconds: number): Promise<void> {
    return this.b.cancel(taskId, graceSeconds);
  }
  destroy(taskId: string): Promise<void> {
    return this.b.destroy(taskId);
  }
  list(): Promise<TaskStatus[]> {
    return this.b.list();
  }
  /** Tells the provider that the session started, so it drops the bootstrap token. */
  ack(taskId: string): Promise<void> {
    return this.b.ack(taskId);
  }
}

/**
 * The provider for a resolved target. `workerId` is set for a worker or a
 * granted pool slot. Throws OfError invalid_request for an unknown provider.
 */
export function providerFor(env: Env, teamId: string, providerId: string, workerId?: string): SandboxProvider {
  if (workerId) return workerProvider(env, teamId, workerId);
  const f = factories.get(providerId);
  if (f) return f(env, teamId);
  if (providerId === "cloudflare-sandbox")
    return new ServiceBindingProvider(providerId, env.PROVIDER_CLOUDFLARE_SANDBOX);
  throw new OfError("invalid_request", `sandbox provider ${JSON.stringify(providerId)} is not available`);
}
