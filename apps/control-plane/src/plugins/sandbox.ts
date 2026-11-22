// Adapts a sandbox plugin Worker to the SandboxProvider contract. The provider
// id is the plugin name.

import type { SandboxPlugin } from "@openfactory/plugin-api";
import type { SandboxProvider } from "@openfactory/sandbox";

/**
 * Wraps an RPC stub. Each method is forwarded explicitly because the members of an
 * RPC stub cannot be spread.
 */
export function adaptSandboxPlugin(name: string, p: SandboxPlugin): SandboxProvider {
  return {
    id: name,
    capabilities: () => p.capabilities(),
    preflight: (task) => p.preflight(task),
    start: (task) => p.start(task),
    status: (taskId) => p.status(taskId),
    cancel: (taskId, graceSeconds) => p.cancel(taskId, graceSeconds),
    destroy: (taskId) => p.destroy(taskId),
    list: () => p.list(),
  };
}
