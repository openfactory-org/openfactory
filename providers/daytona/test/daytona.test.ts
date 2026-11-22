import { describe, expect, it } from "vitest";
import { FetchMock, describeProviderConformance, sampleTask } from "@openfactory/sandbox/testing";
import {
  DaytonaProvider,
  domainAllowList,
  mapDaytonaSandbox,
  type DaytonaConfig,
  type DaytonaSandbox,
} from "../src/index";
import { parseProviderError, type TaskNetworkPolicy } from "@openfactory/sandbox";
import { API, FAKE_KEY, TOOLBOX, fakeDaytona } from "./fake-daytona";

const INSTALL = "inst-test";
const policy: TaskNetworkPolicy = {
  mode: "restricted",
  allowedHosts: ["api.example.com", "pypi.org"],
  deniedHosts: ["pypi.org"],
  alwaysAllow: [".control.example.test", "*.forge.example", "github.com"],
};

function provider(over: Partial<DaytonaConfig> = {}, f = fakeDaytona()) {
  return {
    f,
    p: new DaytonaProvider({
      apiKey: FAKE_KEY,
      installId: INSTALL,
      tier: 3,
      apiUrl: API,
      toolboxUrl: TOOLBOX,
      fetch: f.fetch,
      ...over,
    }),
  };
}

describeProviderConformance("daytona (fetch mock)", async () => ({ provider: provider().p, task: sampleTask }));

describe.skipIf(!process.env.OF_TEST_DAYTONA_KEY || !process.env.OF_TEST_DAYTONA_SNAPSHOT)("daytona live", () => {
  describeProviderConformance("daytona (live)", async () => ({
    provider: new DaytonaProvider({
      apiKey: process.env.OF_TEST_DAYTONA_KEY!,
      installId: `of-test-${Date.now()}`,
      tier: Number(process.env.OF_TEST_DAYTONA_TIER ?? "3"),
      target: process.env.OF_TEST_DAYTONA_TARGET ?? "eu",
    }),
    live: true,
    task: (o) => sampleTask({ image: process.env.OF_TEST_DAYTONA_SNAPSHOT!, ...o }),
  }));
});

describe("egress planning", () => {
  it("maps a policy to a domain list", () => {
    expect(domainAllowList(policy)).toEqual({
      list: ["*.control.example.test", "*.forge.example", "api.example.com", "github.com"],
    });
  });
  it("refuses IP literals and lists above 100 entries", () => {
    expect(domainAllowList({ ...policy, allowedHosts: ["203.0.113.7"] }).reason).toMatch(/IP literal/);
    const many = Array.from({ length: 101 }, (_, i) => `h${i}.example.com`);
    expect(domainAllowList({ mode: "restricted", allowedHosts: many, deniedHosts: [] }).reason).toMatch(/101 entries/);
  });
  it.each<[Partial<DaytonaConfig>, TaskNetworkPolicy | undefined, string]>([
    [{}, undefined, "none"],
    [{}, policy, "native"],
    [{}, { ...policy, allowedHosts: ["203.0.113.7"] }, "in-sandbox"],
    [{ inSandboxLock: false }, { ...policy, allowedHosts: ["203.0.113.7"] }, "none"],
    [{ egress: "in-sandbox" }, policy, "in-sandbox"],
    [{ egress: "none" }, policy, "none"],
    [{ tier: 2 }, policy, "none"],
  ])("%j", (cfg, pol, want) => {
    expect(provider(cfg).p.planEgress(pol).egress).toBe(want);
  });
  it("egress=native refuses a policy that does not fit", () => {
    expect(() => provider({ egress: "native" }).p.planEgress({ ...policy, allowedHosts: ["10.0.0.1"] })).toThrow(
      /IP literal/,
    );
  });
});

describe("create request", () => {
  it("matches the golden body for native egress", async () => {
    const { p } = provider();
    const t = sampleTask({
      taskId: "run_aaaa.1",
      networkPolicy: policy,
      resources: { vcpus: 2, memoryMiB: 4096 },
      maxSessionSeconds: 3600,
      failureRetentionSeconds: 600,
    });
    const body = await p.createBody(t);
    expect({ ...body, env: undefined }).toEqual({
      name: "of-run-aaaa-1",
      snapshot: "of-sandbox-test",
      user: "root",
      target: "eu",
      cpu: 2,
      memory: 4,
      disk: 3,
      env: undefined,
      labels: { "of-install": INSTALL, "of-task": "run-aaaa-1", "of-team": "default" },
      autoStopInterval: 0,
      autoArchiveInterval: 0,
      autoDeleteInterval: 10,
      ttlMinutes: 75,
      queueTimeout: 10,
      public: false,
      domainAllowList: "*.control.example.test,*.forge.example,api.example.com,github.com",
    });
    expect((body.env as Record<string, string>).OF_EGRESS_LOCK).toBe("none");
  });
  it("in-sandbox egress sets the lock and the daemon ports, and no domain list", async () => {
    const { p } = provider({ egress: "in-sandbox" });
    const body = await p.createBody(sampleTask({ networkPolicy: policy }));
    expect(body.domainAllowList).toBeUndefined();
    expect(body.env).toMatchObject({ OF_EGRESS_LOCK: "iptables", OF_LOOPBACK_DAEMON_PORTS: "2280" });
  });
  it("an existing name returns the sandbox (the name is the idempotency key)", async () => {
    const { f, p } = provider();
    const t = sampleTask();
    await p.start(t);
    f.api.once("GET", "/sandbox/:name", () => FetchMock.json({ statusCode: 404, message: "not found" }, 404));
    const s = await p.start(t);
    expect(s.sandboxId).toBe("00000000-0000-0000-0000-000000000001");
    expect(f.sandboxes.size).toBe(1);
  });
  it.each([
    [429, "Too many requests", "rate_limited", { "retry-after-sandbox-create": "20" }, 20],
    [403, "Total CPU quota exceeded", "at_capacity", {}, undefined],
    [400, "Invalid snapshot", "invalid", {}, undefined],
    [400, "Network settings cannot be changed on tier 1", "invalid", {}, undefined],
    [502, "Bad gateway", "unavailable", {}, undefined],
  ] as const)("%d %s → %s", async (status, message, kind, headers, retry) => {
    const { f, p } = provider();
    f.api.on("POST", "/sandbox", () => FetchMock.json({ statusCode: status, message }, status, headers));
    const pe = parseProviderError(await p.start(sampleTask()).catch((e: unknown) => e))!;
    expect(pe.kind).toBe(kind);
    expect(pe.retryAfterSeconds).toBe(retry);
  });
});

describe("preflight", () => {
  it("refuses tier 1 and 2", async () => {
    const r = await provider({ tier: 1 }).p.preflight(sampleTask());
    expect(r).toMatchObject({ ok: false, code: "invalid_configuration" });
    if (!r.ok) expect(r.message).toMatch(/tier 1/);
  });
  it("reactivates an inactive snapshot", async () => {
    const { f, p } = provider();
    f.snapshots.set("of-sandbox-test", { id: "snap1", state: "inactive" });
    expect(await p.preflight(sampleTask())).toMatchObject({ ok: true });
    expect(f.api.find("POST", "/snapshots/:id/activate")).toHaveLength(1);
  });
  it.each([
    ["error", "invalid_configuration"],
    ["building", "sandbox_create_failed"],
  ])("a snapshot in %s → %s", async (state, code) => {
    const { f, p } = provider();
    f.snapshots.set("of-sandbox-test", { id: "snap1", state });
    expect(await p.preflight(sampleTask())).toMatchObject({ ok: false, code });
  });
  it("refuses a missing snapshot", async () => {
    expect(await provider().p.preflight(sampleTask({ image: "nope" }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
  });
  it("refuses more than the org maximum", async () => {
    expect(await provider().p.preflight(sampleTask({ resources: { vcpus: 8 } }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
  });
  it("refuses a restricted policy it cannot enforce unless allowed", async () => {
    const t = sampleTask({ networkPolicy: { ...policy, allowedHosts: ["10.0.0.1"] } });
    expect(await provider({ inSandboxLock: false }).p.preflight(t)).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
    expect(await provider({ inSandboxLock: false, allowUnenforcedNetworkPolicy: "true" }).p.preflight(t)).toMatchObject(
      { ok: true },
    );
  });
  it("creates a missing snapshot with the bootstrap entrypoint", async () => {
    const { f, p } = provider();
    await p.ensureSnapshot({ name: "of-sandbox-2", imageName: "ghcr.io/openfactory-org/sandbox:2" });
    expect(JSON.parse(f.api.find("POST", "/snapshots")[0]!.body)).toMatchObject({
      entrypoint: ["/usr/bin/tini", "-g", "--", "/opt/openfactory/bootstrap.sh"],
      sandboxClass: "container",
    });
  });
});

describe("state mapping", () => {
  const sb = (o: Partial<DaytonaSandbox>): DaytonaSandbox => ({ id: "s1", state: "started", ...o });
  const now = Date.parse("2026-11-22T12:00:00Z");
  it.each<
    [
      string,
      Partial<DaytonaSandbox>,
      { exitCode?: number; cancelRequested?: boolean; nativeEgress?: boolean },
      string,
      string | undefined,
    ]
  >([
    ["creating", { state: "creating" }, {}, "starting", undefined],
    ["pulling", { state: "pulling_snapshot" }, {}, "starting", undefined],
    ["queue timeout", { state: "creating", queueTimedOutAt: "x" }, {}, "exited", "unschedulable"],
    ["build failed", { state: "build_failed" }, {}, "exited", "image_pull"],
    ["started", {}, {}, "running", undefined],
    ["exit 0", {}, { exitCode: 0 }, "exited", "completed"],
    ["exit 75", {}, { exitCode: 75 }, "exited", "network_policy_timeout"],
    ["exit 64", {}, { exitCode: 64 }, "exited", "invalid_spec"],
    ["exit 137 oom", { errorReason: "OOMKilled" }, { exitCode: 137 }, "exited", "container_oom"],
    ["exit 2", {}, { exitCode: 2 }, "exited", "process_failed"],
    ["policy error", { networkPolicyError: "bad list" }, { nativeEgress: true }, "exited", "network_policy_timeout"],
    ["stopped after cancel", { state: "stopped" }, { cancelRequested: true }, "exited", "cancelled"],
    [
      "destroyed by ttl",
      { state: "destroyed", autoDestroyAt: "2026-11-22T11:00:00Z" },
      {},
      "exited",
      "deadline_exceeded",
    ],
    ["spot evicted", { state: "stopped", spotEvictedAt: "x" }, {}, "exited", "evicted"],
    ["stopped, other", { state: "archived" }, {}, "exited", "evicted"],
    ["error recoverable", { state: "error", recoverable: true }, {}, "exited", "node_lost"],
    ["error", { state: "error" }, {}, "exited", "unknown"],
    ["resizing", { state: "resizing" }, {}, "running", undefined],
    ["paused", { state: "paused" }, {}, "exited", "evicted"],
  ])("%s", (_n, over, ctx, state, reason) => {
    const s = mapDaytonaSandbox("t.1", sb(over), { ...ctx, now });
    expect(s.state).toBe(state);
    expect(s.reason).toBe(reason);
  });

  it("reads the exit code from the toolbox entrypoint session", async () => {
    const { f, p } = provider();
    const t = sampleTask();
    const st = await p.start(t);
    f.sandboxes.get(`of-${t.taskId.replace(/[_.]/g, "-")}`)!.state = "started";
    expect((await p.status(t.taskId)).state).toBe("running");
    f.exitCodes.set(st.sandboxId!, 75);
    expect(await p.status(t.taskId)).toMatchObject({ state: "exited", reason: "network_policy_timeout", exitCode: 75 });
  });
});

describe("cancel, destroy and list", () => {
  it("cancel sends SIGTERM through the toolbox", async () => {
    const { f, p } = provider();
    const t = sampleTask();
    await p.start(t);
    for (const v of f.sandboxes.values()) v.state = "started";
    await p.cancel(t.taskId, 30);
    expect(f.executed[0]!.body).toEqual({ command: "kill -TERM $(cat /run/openfactory/runner.pid)", timeout: 10 });
    for (const v of f.sandboxes.values()) v.state = "stopped";
    expect((await p.status(t.taskId)).reason).toBe("cancelled");
  });
  it("the cancel mark lives on the sandbox, not in the provider object", async () => {
    // The control plane builds a new provider object for each call.
    const f = fakeDaytona();
    const t = sampleTask();
    await provider({}, f).p.start(t);
    for (const v of f.sandboxes.values()) v.state = "started";
    await provider({}, f).p.cancel(t.taskId, 30);
    const sb = [...f.sandboxes.values()][0]!;
    expect(sb.labels).toMatchObject({ "of-install": INSTALL, "of-cancel": "1" });
    sb.state = "stopped";
    expect((await provider({}, f).p.status(t.taskId)).reason).toBe("cancelled");
    expect((await provider({}, f).p.list())[0]!.reason).toBe("cancelled");
    // Without a cancel, a stopped sandbox is evicted.
    const t2 = sampleTask();
    await provider({}, f).p.start(t2);
    for (const v of f.sandboxes.values()) if (v.labels?.["of-cancel"] !== "1") v.state = "stopped";
    expect((await provider({}, f).p.status(t2.taskId)).reason).toBe("evicted");
  });
  it("list sends the labels as a JSON string", async () => {
    const { f, p } = provider();
    const t = sampleTask();
    await p.start(t);
    const listed = await p.list();
    expect(listed.map((x) => x.taskId)).toEqual([t.taskId]);
    expect(new URL(f.api.find("GET", "/sandbox")[0]!.url).searchParams.get("labels")).toBe(
      JSON.stringify({ "of-install": INSTALL }),
    );
  });
});
