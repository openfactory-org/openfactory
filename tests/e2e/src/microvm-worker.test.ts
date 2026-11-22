// Live: a microVM worker on a KVM host. Acceptance scenario 7 (enroll, run,
// drain, revoke).
//
//   OF_E2E_KVM_WORKER=1        the host has /dev/kvm and openfactory-runtime
//   OF_E2E_WORKER_ENROLL_CMD   the enroll command, with {token} and {url}
//                              placeholders, for example
//                              "sudo openfactory-runtime worker enroll --token {token} --control-url {url}"
//   OF_E2E_ANTHROPIC_API_KEY   model credential for the claude harness
import { execSync } from "node:child_process";
import { beforeAll, expect, it } from "vitest";
import {
  api,
  describeLive,
  ensureModelSecret,
  env,
  fixture,
  live,
  renameFactory,
  uniq,
  waitFor,
  waitRunDone,
} from "./live.ts";

interface Worker {
  id: string;
  name: string;
  status?: string;
}

describeLive(["OF_E2E_KVM_WORKER", "OF_E2E_WORKER_ENROLL_CMD", "OF_E2E_ANTHROPIC_API_KEY"], "microVM worker", () => {
  const name = uniq("e2e-vm");
  let worker: Worker;

  beforeAll(async () => {
    await ensureModelSecret();
  });

  it(
    "enrolls and comes online",
    async () => {
      const enrollment = await api<{ token: string }>("POST", "/workers/enrollments", {
        name,
        labels: { e2e: "true" },
      });
      const cmd = env("OF_E2E_WORKER_ENROLL_CMD").replace("{token}", enrollment.token).replace("{url}", live.baseUrl);
      execSync(cmd, { stdio: "inherit" });
      worker = await waitFor(
        "worker online",
        async () => {
          const list = await api<{ items: Worker[] }>("GET", "/workers");
          return list.items.find((w) => w.name === name && w.status === "online");
        },
        5 * 60_000,
      );
      expect(worker.id).toBeTruthy();
    },
    10 * 60_000,
  );

  it(
    "runs a turn on the worker",
    async () => {
      const created = await api<{ factory: { uid: string } }>("POST", "/factories", {
        files: renameFactory(fixture("def"), uniq("e2e-vm-f")),
      });
      const run = await api<{ run_id: string }>("POST", "/runs", {
        prompt: "Reply with exactly the text E2E-VM-OK and nothing else.",
        factory_id: created.factory.uid,
        agent_name: "foreman",
        config: { worker_host: worker.name },
      });
      await waitRunDone(run.run_id);
      const msgs = await api<{ messages: { role: string; content: string }[] }>("GET", `/runs/${run.run_id}/messages`);
      expect(msgs.messages.some((m) => m.content.includes("E2E-VM-OK"))).toBe(true);
    },
    30 * 60_000,
  );

  it(
    "drains, resumes and revokes",
    async () => {
      const drained = await api<Worker>("POST", `/workers/${worker.id}/drain`, {});
      expect(drained.status).toBe("draining");
      await api("POST", `/workers/${worker.id}/resume`, {});
      const revoked = await api<Worker>("POST", `/workers/${worker.id}/revoke`, {});
      expect(revoked.status).toBe("revoked");
    },
    5 * 60_000,
  );
});
