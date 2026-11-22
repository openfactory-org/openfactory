import { describe, expect, it } from "vitest";
import { describeProviderConformance, sampleTask } from "@openfactory/sandbox/testing";
import { REPORT_HOST, blockedMessage, decideEgress, recordBlocked, type EgressProps } from "../src/egress";
import { Egress, SandboxProvider } from "../src/index";
import { CF_INSTANCE_TYPES } from "../src/provider";
import type { TaskNetworkPolicy } from "@openfactory/sandbox";
import { BOOTSTRAP_ENTRYPOINT } from "@openfactory/sandbox/provider-kit";
import { BOOT_GRACE_MS, DEADLINE_SLACK_MS, INACTIVITY_TIMEOUT_MS, KEEPALIVE_MS } from "../src/task-sandbox";
import { fakeProvider, harness } from "./fakes";

const small = CF_INSTANCE_TYPES[1]!;
const policy: TaskNetworkPolicy = {
  mode: "restricted",
  allowedHosts: ["API.example.com", "registry.npmjs.org", "blocked.example.com"],
  deniedHosts: ["blocked.example.com", "github.com"],
  alwaysAllow: ["github.com", ".openfactory.internal", "*.forge.example"],
};

describeProviderConformance("cloudflare-sandbox (fake container)", async () => ({
  provider: fakeProvider().provider,
  task: sampleTask,
}));

describe("Egress decisions (netguard cases)", () => {
  const props: EgressProps = {
    taskId: "t.1",
    controlHost: "control.example.test",
    policy,
    bootstrapHosts: ["acct.r2.cloudflarestorage.com"],
  };
  it.each([
    ["https://control.example.test/runner/v1/sessions/start", "control"],
    [`http://${REPORT_HOST}/blocked`, "report"],
    ["https://api.example.com/v1", "forward"],
    ["https://API.EXAMPLE.COM./x", "forward"],
    ["https://sub.api.example.com/", "blocked"],
    ["https://blocked.example.com/", "blocked"],
    ["https://github.com/org/repo.git", "forward"],
    ["https://control.openfactory.internal/", "forward"],
    ["https://openfactory.internal/", "forward"],
    ["https://git.forge.example/", "forward"],
    ["https://forge.example/", "blocked"],
    ["https://203.0.113.7/", "blocked"],
    ["https://acct.r2.cloudflarestorage.com/runner", "forward"],
    ["http://[::1]:80/", "blocked"],
  ])("%s → %s", (url, kind) => {
    expect(decideEgress(props, url).kind).toBe(kind);
  });
  it("an unrestricted task forwards everything except denied hosts", () => {
    const p: EgressProps = { taskId: "t", controlHost: "c.test" };
    expect(decideEgress(p, "https://anything.example/").kind).toBe("forward");
  });
  it("records blocked hosts like netguard", () => {
    let list = recordBlocked([], "B.example:443", new Date(1000));
    list = recordBlocked(list, "a.example", new Date(2000));
    list = recordBlocked(list, "b.example", new Date(3000));
    expect(list).toEqual([
      { host: "b.example", count: 2, first: new Date(1000).toISOString(), last: new Date(3000).toISOString() },
      { host: "a.example", count: 1, first: new Date(2000).toISOString(), last: new Date(2000).toISOString() },
    ]);
    expect(blockedMessage("X.example")).toBe("Open Factory network policy blocked access to x.example\n");
  });
});

describe("Egress entrypoint", () => {
  function egress(props: EgressProps) {
    const calls: string[] = [];
    const recorded: string[] = [];
    const stub = {
      recordBlocked: async (h: string) => void recorded.push(h),
      blockedReport: async () => ({ blocked: [{ host: "x.example", count: 1, first: "a", last: "a" }] }),
    };
    const env = {
      CONTROL_PLANE: { fetch: async (r: Request) => (calls.push(r.url), new Response("control")) },
      SANDBOXES: { idFromName: (n: string) => n, get: () => stub },
    };
    const ctx = { props, waitUntil: (p: Promise<unknown>) => void p };
    return { e: new Egress(ctx as never, env as never), calls, recorded };
  }
  const props: EgressProps = { taskId: "t.1", controlHost: "control.example.test", policy };

  it("routes the control host through the service binding", async () => {
    const { e, calls } = egress(props);
    expect(await (await e.fetch(new Request("https://control.example.test/runner/v1/x"))).text()).toBe("control");
    expect(calls).toEqual(["https://control.example.test/runner/v1/x"]);
  });
  it("answers the blocked report", async () => {
    const { e } = egress(props);
    expect(await (await e.fetch(new Request(`http://${REPORT_HOST}/blocked`))).json()).toMatchObject({
      blocked: [{ host: "x.example" }],
    });
  });
  it("blocks with 403 and records the host", async () => {
    const { e, recorded } = egress(props);
    const res = await e.fetch(new Request("https://evil.example/"));
    expect(res.status).toBe(403);
    expect(await res.text()).toBe(blockedMessage("evil.example"));
    expect(recorded).toEqual(["evil.example"]);
  });
});

describe("OfTaskSandbox state machine", () => {
  it("registers the intercepts before start, with enableInternet false", async () => {
    const h = harness();
    const t = sampleTask({ networkPolicy: policy });
    const s = await h.sandbox.start(t, small);
    expect(s.state).toBe("running");
    expect(h.container.log.slice(0, 3)).toEqual(["intercept:http", "intercept:https:*", "start"]);
    expect(h.container.started).toMatchObject({
      entrypoint: [...BOOTSTRAP_ENTRYPOINT],
      enableInternet: false,
      instance: "basic",
      env: {
        OF_EGRESS_LOCK: "none",
        OF_TRUST_CA: "/etc/cloudflare/certs/cloudflare-containers-ca.crt",
        OF_NETGUARD_REPORT: "http://of-egress.internal/blocked",
        OF_BOOTSTRAP_TOKEN: t.env.OF_BOOTSTRAP_TOKEN,
      },
    });
    expect(h.container.started!.labels!.t).toMatch(/^[0-9a-f]{16}$/);
    expect(h.egressProps[0]).toMatchObject({
      taskId: t.taskId,
      controlHost: "control.example.test",
      policy,
      bootstrapHosts: ["control.example.test"],
    });
    expect(h.container.log).toContain(`timeout:${INACTIVITY_TIMEOUT_MS}`);
    expect(h.alarms).toEqual([h.clock.now + KEEPALIVE_MS]);
    expect(h.registry.list().taskIds).toEqual([t.taskId]);
  });

  it("start is idempotent", async () => {
    const h = harness();
    const t = sampleTask();
    await h.sandbox.start(t, small);
    await h.sandbox.start(t, small);
    expect(h.container.log.filter((l) => l === "start")).toHaveLength(1);
  });

  it("passes custom resources", async () => {
    const h = harness();
    await h.sandbox.start(sampleTask(), { name: "custom", vcpus: 3, memoryMiB: 6000, diskMiB: 10000 });
    expect(h.container.started!.instance).toEqual({ vcpu: 3, memoryMib: 6000, diskMb: 10000 });
  });

  it("refuses an IP literal in the allow list", async () => {
    const h = harness();
    await expect(
      h.sandbox.start(sampleTask({ networkPolicy: { ...policy, allowedHosts: ["203.0.113.7"] } }), small),
    ).rejects.toMatchObject({ kind: "invalid" });
    expect(h.container.log).toEqual([]);
  });

  it("maps a TypeError from start to invalid_spec", async () => {
    const h = harness();
    h.container.startError = new TypeError("bad instance");
    expect(await h.sandbox.start(sampleTask(), small)).toMatchObject({ state: "exited", reason: "invalid_spec" });
  });

  it("a missing image is image_pull", async () => {
    const h = harness();
    h.container.images = {};
    expect(await h.sandbox.start(sampleTask(), small)).toMatchObject({ state: "exited", reason: "image_pull" });
  });

  it.each([
    [0, "completed"],
    [75, "network_policy_timeout"],
    [64, "invalid_spec"],
    [65, "invalid_spec"],
    [1, "process_failed"],
  ])("monitor() exit %d → %s", async (code, reason) => {
    const h = harness();
    await h.sandbox.start(sampleTask(), small);
    h.container.exit(code);
    await h.flush();
    expect(h.sandbox.status()).toMatchObject({ state: "exited", reason, exitCode: code });
  });

  it("monitor() errors while booting map to image_pull and unschedulable", async () => {
    for (const [msg, reason] of [
      ["failed to pull image", "image_pull"],
      ["no capacity available", "unschedulable"],
    ] as const) {
      const h = harness();
      await h.sandbox.start(sampleTask(), small);
      h.container.running = false;
      h.container.exit(-1, msg);
      // exit() with a non-zero code adds exitCode; drop it to model an error without one.
      await h.flush();
      const rec = h.kv.get<{ outcome: { exitCode?: number }; seenRunning?: boolean }>("task")!;
      delete rec.outcome.exitCode;
      rec.seenRunning = false;
      h.kv.put("task", rec);
      expect(h.sandbox.status().reason).toBe(reason);
    }
  });

  it("is starting while booting, then evicted when the container never runs", async () => {
    const h = harness();
    await h.sandbox.start(sampleTask(), small);
    h.container.running = false;
    const rec = h.kv.get<{ seenRunning?: boolean }>("task")!;
    delete rec.seenRunning;
    h.kv.put("task", rec);
    expect(h.sandbox.status().state).toBe("starting");
    h.clock.now += BOOT_GRACE_MS;
    expect(h.sandbox.status()).toMatchObject({ state: "exited", reason: "evicted" });
  });

  it("the alarm detects a host restart (sandbox_lost) and keeps the timeout", async () => {
    const h = harness();
    await h.sandbox.start(sampleTask(), small);
    h.clock.now += KEEPALIVE_MS;
    await h.sandbox.alarm();
    expect(h.container.log.filter((l) => l.startsWith("timeout:")).length).toBe(2);
    h.container.vanish();
    h.clock.now += KEEPALIVE_MS;
    await h.sandbox.alarm();
    expect(h.sandbox.status()).toMatchObject({ state: "exited", reason: "evicted" });
  });

  it("the alarm enforces the deadline: SIGTERM, then destroy", async () => {
    const h = harness();
    const t = sampleTask({ maxSessionSeconds: 60 });
    await h.sandbox.start(t, small);
    h.clock.now += 60_000 + DEADLINE_SLACK_MS;
    await h.sandbox.alarm();
    expect(h.container.log).toContain("signal:15");
    h.clock.now += KEEPALIVE_MS;
    await h.sandbox.alarm();
    expect(h.container.log).toContain("destroy:deadline");
    expect(h.sandbox.status()).toMatchObject({ state: "exited", reason: "deadline_exceeded" });
  });

  it("cancel signals, the alarm destroys after the grace, and retention cleans up", async () => {
    const h = harness();
    const t = sampleTask({ failureRetentionSeconds: 600 });
    await h.sandbox.start(t, small);
    await h.sandbox.cancel(10);
    expect(h.container.log).toContain("signal:15");
    expect(h.alarms.at(-1)).toBe(h.clock.now + 10_000);
    h.clock.now += 10_000;
    await h.sandbox.alarm();
    expect(h.sandbox.status()).toMatchObject({ state: "exited", reason: "cancelled" });
    expect(h.alarms.at(-1)).toBe(h.clock.now + 600_000);
    h.clock.now += 600_000;
    await h.sandbox.alarm();
    expect(() => h.sandbox.status()).toThrow(/no such task/);
    expect(h.registry.list().taskIds).toEqual([]);
  });

  it("ack forgets the bootstrap token; onStartup restores the timeout", async () => {
    const h = harness();
    await h.sandbox.start(sampleTask(), small);
    h.sandbox.ack();
    expect(h.kv.get<{ task: { env: Record<string, string> } }>("task")!.task.env.OF_BOOTSTRAP_TOKEN).toBeUndefined();
    const before = h.container.log.length;
    await h.sandbox.onStartup();
    expect(h.container.log.slice(before)).toEqual([`timeout:${INACTIVITY_TIMEOUT_MS}`]);
  });

  it("an alarm never throws and always sets the next alarm", async () => {
    const h = harness();
    await h.sandbox.start(sampleTask(), small);
    h.container.setInactivityTimeout = () => Promise.reject(new Error("platform down"));
    const n = h.alarms.length;
    await h.sandbox.alarm();
    expect(h.alarms.length).toBe(n + 1);
  });
});

describe("provider", () => {
  it("preflight refuses IP literals and a runner version mismatch", async () => {
    const { provider } = fakeProvider();
    expect(
      await provider.preflight(sampleTask({ networkPolicy: { ...policy, allowedHosts: ["10.0.0.1"] } })),
    ).toMatchObject({ ok: false, code: "invalid_configuration" });
    const t = sampleTask();
    t.env.OF_RUNNER_VERSION = "9.9.9";
    expect(await provider.preflight(t)).toMatchObject({ ok: false, code: "invalid_configuration" });
  });
  it("preflight maps machine size to an instance type", async () => {
    const { provider } = fakeProvider();
    expect(await provider.preflight(sampleTask({ resources: { vcpus: 2, memoryMiB: 8192 } }))).toMatchObject({
      ok: true,
      resources: { instanceType: "custom" },
    });
    expect(await provider.preflight(sampleTask({ resources: { vcpus: 5 } }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
  });
  it("the WorkerEntrypoint exposes id() for the RPC shape", async () => {
    const ep = new SandboxProvider({} as never, {} as never);
    expect(await ep.id()).toBe("cloudflare-sandbox");
  });
  it("list drops stale registry rows", async () => {
    const { provider, registry } = fakeProvider();
    registry.add("run_stale.1", 0);
    expect(await provider.list()).toEqual([]);
    expect(registry.list().taskIds).toEqual([]);
  });
});

describe.skipIf(!process.env.OF_TEST_CF_ACCOUNT_ID || !process.env.OF_TEST_CF_API_TOKEN)(
  "cloudflare-sandbox live",
  () => {
    it("needs a deployed test copy (wrangler deploy --name of-provider-test); run the conformance suite against it from the control plane tests", () => {
      expect(process.env.OF_TEST_CF_ACCOUNT_ID).toBeTruthy();
    });
  },
);
