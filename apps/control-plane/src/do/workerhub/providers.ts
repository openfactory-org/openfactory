// Sandbox providers backed by workers:
// - "local": development process workers (OF_SANDBOX_PROVIDER=local), registered
//   in the DispatchDO provider registry by bootstrapControlPlane (src/wiring.ts).
// - workerProvider(): the microvm provider of one enrolled worker, for
//   providerFor() when a grant names a worker.
import { doName, newId } from "@openfactory/core";
import type { Arch, SandboxProvider } from "@openfactory/sandbox";
import { LocalProvider, type DevWorker } from "@openfactory/sandbox/local";
import { MicroVMProvider, type WorkerShape } from "@openfactory/sandbox/microvm";
import type { TeamRpc } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { unwrapOp } from "../../lib/rpc";
import type { WorkerRecord, WorkerStatus } from "../team/workers";

type ListedWorker = WorkerRecord & { status: WorkerStatus; slotsUsed: number; arch: string };

async function listWorkers(env: Env, teamId: string): Promise<ListedWorker[]> {
  const r = await (env.TEAM.getByName(doName.team(teamId)) as unknown as TeamRpc).api(
    { requestId: newId("req") },
    "workers.list",
    {},
    null,
  );
  return unwrapOp<ListedWorker[]>(r, "workers.list");
}

function archOf(a: string): Arch[] {
  return a === "amd64" || a === "arm64" ? [a] : [];
}

function hubClient(env: Env, teamId: string, workerId: string) {
  return env.WORKER_HUB.getByName(doName.workerHub(teamId, workerId));
}

/** The local provider over the team's development process workers. */
export function localProvider(env: Env, teamId: string): SandboxProvider {
  return new LocalProvider({
    list: async (): Promise<DevWorker[]> =>
      (await listWorkers(env, teamId))
        .filter((w) => w.kind === "process" && w.status !== "revoked")
        .map((w) => ({
          id: w.id,
          online: w.status === "online" || w.status === "draining",
          draining: w.draining,
          slots: w.slots,
          used: w.slotsUsed,
          arch: archOf(w.arch),
        })),
    hub: (id) => hubClient(env, teamId, id),
  });
}

/** The microvm provider of one worker; capabilities and preflight come from its record. */
export function workerProvider(env: Env, teamId: string, workerId: string): SandboxProvider {
  let cached: WorkerShape | undefined;
  return new MicroVMProvider(hubClient(env, teamId, workerId), async () => {
    if (cached) return cached;
    const w = (await listWorkers(env, teamId)).find((x) => x.id === workerId);
    cached = {
      arch: archOf(w?.arch ?? ""),
      slots: w?.slots ?? 0,
      ...(w?.slotSize
        ? {
            slotSize: {
              vcpus: w.slotSize.vcpus,
              memoryMiB: w.slotSize.memory_mib,
              ...(w.slotSize.scratch_gib ? { scratchGiB: w.slotSize.scratch_gib } : {}),
            },
          }
        : {}),
    };
    return cached;
  });
}
