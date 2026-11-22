import { execFileSync } from "node:child_process";
import { describe, expect, it } from "vitest";
import { FetchMock, describeProviderConformance, sampleTask } from "@openfactory/sandbox/testing";
import {
  BoatdevProvider,
  exitCodeFromSetupError,
  mapBoatSandbox,
  setupScript,
  type BoatSandbox,
  type RateLimitInfo,
} from "../src/index";
import { parseProviderError, type TaskNetworkPolicy } from "@openfactory/sandbox";
import { FAKE_BASE, FAKE_KEY, fakeBoat } from "./fake-boat";

const INSTALL = "inst-test";
const policy: TaskNetworkPolicy = { mode: "restricted", allowedHosts: ["api.example.com"], deniedHosts: [] };

function provider(f = fakeBoat(), extra: Partial<ConstructorParameters<typeof BoatdevProvider>[0]> = {}) {
  return {
    f,
    p: new BoatdevProvider({ apiKey: FAKE_KEY, installId: INSTALL, baseUrl: FAKE_BASE, fetch: f.mock.fetch, ...extra }),
  };
}

describeProviderConformance("boatdev (fetch mock)", async () => {
  const { p } = provider();
  return { provider: p, task: (o) => sampleTask({ image: "of-sandbox-test", ...o }) };
});

describe.skipIf(!process.env.OF_TEST_BOAT_KEY || !process.env.OF_TEST_BOAT_SNAPSHOT)("boatdev live", () => {
  describeProviderConformance("boatdev (live)", async () => {
    const p = new BoatdevProvider({ apiKey: process.env.OF_TEST_BOAT_KEY!, installId: `of-test-${Date.now()}` });
    return { provider: p, live: true, task: (o) => sampleTask({ image: process.env.OF_TEST_BOAT_SNAPSHOT!, ...o }) };
  });
});

describe("create request", () => {
  it("matches the golden body, with the token redacted", async () => {
    const { p } = provider();
    const t = sampleTask({
      runId: "run_aaaaaaaaaaaaaaaabbbbbbbbbbbb",
      taskId: "run_aaaaaaaaaaaaaaaabbbbbbbbbbbb.1",
      networkPolicy: policy,
      maxSessionSeconds: 7200,
    });
    const body = await p.createBody(t);
    const env = body.env as Record<string, string>;
    expect(env.OF_BOOTSTRAP_TOKEN).toMatch(/^ofb_/);
    env.OF_BOOTSTRAP_TOKEN = "<redacted>";
    expect({ ...body, setupScript: "<script>" }).toEqual({
      name: "of-run-aaaaaaaaaaaaaaaabbbbbbbbbbbb-1",
      from: "of-sandbox-test",
      type: "small",
      ttlSeconds: 8100,
      noEnv: true,
      snapshots: false,
      monitoring: true,
      setupScript: "<script>",
      labels: { "of-install": INSTALL, "of-task": "run-aaaaaaaaaaaaaaaabbbbbbbbbbbb-1", "of-team": "default" },
      env: {
        OF_CONTROL_URL: "https://control.example.test",
        OF_RUN_ID: "run_aaaaaaaaaaaaaaaabbbbbbbbbbbb",
        OF_SESSION: "1",
        OF_BOOTSTRAP_TOKEN: "<redacted>",
        OF_RUNNER_VERSION: "0.0.0-test",
        OF_RUNNER_URL: "https://control.example.test/runner/v1/bin/linux-amd64",
        OF_RUNNER_SHA256: "0".repeat(64),
        OF_EGRESS_LOCK: "iptables",
        OF_LOOPBACK_DAEMON_PORTS: expect.stringMatching(/^22,6080,8090,8911,47984,.*,48010$/),
        OF_ENV_FILE: "/run/openfactory/runner.env",
      },
    });
    expect(body.setupScript).toContain("exec sudo -n /usr/bin/tini -g -- /opt/openfactory/bootstrap.sh");
  });

  it("an unrestricted task gets no lock", async () => {
    const { p } = provider();
    const env = (await p.createBody(sampleTask())).env as Record<string, string>;
    expect(env.OF_EGRESS_LOCK).toBe("none");
    expect(env.OF_LOOPBACK_DAEMON_PORTS).toBeUndefined();
  });

  it("sends Idempotency-Key = sha256(installId:taskId) and the auth header", async () => {
    const { f, p } = provider();
    const t = sampleTask({ taskId: "run_x.1" });
    await p.start(t);
    const [call] = f.mock.find("POST", "/sandboxes");
    expect(call!.headers["idempotency-key"]).toBe(
      execFileSync("sh", ["-c", `printf %s '${INSTALL}:run_x.1' | shasum -a 256 | cut -d' ' -f1`])
        .toString()
        .trim(),
    );
    expect(call!.headers.authorization).toBe(`Bearer ${FAKE_KEY}`);
  });

  it("refuses a task without a bootstrap token", async () => {
    const { p } = provider();
    const t = sampleTask();
    delete t.env.OF_BOOTSTRAP_TOKEN;
    await expect(p.start(t)).rejects.toMatchObject({ kind: "invalid" });
  });

  it("recovers the sandbox by label on 409 idempotency_key_reused", async () => {
    const f = fakeBoat();
    const t = sampleTask();
    await provider(f).p.start(t);
    // A new provider instance (no cached ID) with a changed body: the key is reused.
    const { p } = provider(f);
    const t2 = { ...t, env: { ...t.env, OF_BOOTSTRAP_TOKEN: "ofb_default.test.other-fake-token" } };
    const s = await p.start(t2);
    expect(s.sandboxId).toBe("sbx_1");
    expect(f.sandboxes.size).toBe(1);
    expect(f.mock.find("GET", "/sandboxes")[0]!.url).toContain("label=of-task%3D");
  });

  it("feeds rate-limit headers back", async () => {
    const seen: RateLimitInfo[] = [];
    const { p } = provider(fakeBoat(), { onRateLimit: (i) => seen.push(i) });
    await p.start(sampleTask());
    expect(seen[0]).toMatchObject({ provider: "boatdev", policy: expect.stringContaining("starts-minute") });
  });
});

describe("setupScript", () => {
  it("is valid sh and writes a quoted env file", () => {
    const s = setupScript(["OF_RUN_ID", "OF_CONTROL_URL", "bad-key"]);
    expect(s).not.toContain("bad-key");
    execFileSync("sh", ["-n", "-c", s]);
    // Run the env-file part with a fake sudo that writes to a temp path.
    const out = execFileSync(
      "sh",
      [
        "-c",
        s
          .split("\n")
          .slice(2, 6)
          .join("\n")
          .replace(/\| sudo -n tee \S+ >\/dev\/null/, ""),
      ],
      {
        env: { PATH: process.env.PATH, OF_RUN_ID: "run_a", OF_CONTROL_URL: "https://x.test/it's" },
      },
    ).toString();
    expect(out).toBe("export OF_CONTROL_URL='https://x.test/it'\\''s'\nexport OF_RUN_ID='run_a'\n");
  });
});

describe("start failures", () => {
  it.each([
    [429, "rate_limited", "rate_limited", { "retry-after": "12" }, 12],
    [429, "limit_reached", "at_capacity", {}, undefined],
    [503, "no_ready_machine", "at_capacity", {}, 30],
    [402, "billing_required", "unavailable", {}, undefined],
    [403, "trial_machine_class_not_allowed", "invalid", {}, undefined],
    [403, "machine_class_plan_required", "invalid", {}, undefined],
    [404, "snapshot_not_found", "invalid", {}, undefined],
    [500, "internal", "unavailable", {}, undefined],
  ] as const)("%d %s → %s", async (status, code, kind, headers, retry) => {
    const { f, p } = provider();
    f.mock.on("POST", "/sandboxes", () =>
      FetchMock.json({ ok: false, code, error: { code, status } }, status, headers),
    );
    const err = await p.start(sampleTask()).catch((e: unknown) => e);
    const pe = parseProviderError(err)!;
    expect(pe.kind).toBe(kind);
    expect(pe.retryAfterSeconds).toBe(retry);
  });

  it("a network failure is unavailable", async () => {
    const p = new BoatdevProvider({
      apiKey: FAKE_KEY,
      installId: INSTALL,
      baseUrl: FAKE_BASE,
      fetch: () => Promise.reject(new Error("reset")),
    });
    await expect(p.start(sampleTask())).rejects.toMatchObject({ kind: "unavailable" });
  });
});

describe("preflight", () => {
  it("passes with a ready snapshot and sets the type", async () => {
    const { p } = provider();
    expect(await p.preflight(sampleTask({ resources: { vcpus: 3 } }))).toMatchObject({
      ok: true,
      resources: { instanceType: "default" },
    });
  });
  it("refuses a missing snapshot", async () => {
    const { p } = provider();
    expect(await p.preflight(sampleTask({ image: "missing" }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
  });
  it("refuses bad credentials", async () => {
    const f = fakeBoat();
    const p = new BoatdevProvider({
      apiKey: "boat_wrong",
      installId: INSTALL,
      baseUrl: FAKE_BASE,
      fetch: f.mock.fetch,
    });
    expect(await p.preflight(sampleTask())).toMatchObject({ ok: false, code: "invalid_configuration" });
  });
  it("refuses xlarge on the default plan", async () => {
    const { p } = provider();
    expect(await p.preflight(sampleTask({ resources: { vcpus: 16 } }))).toMatchObject({
      ok: false,
      code: "invalid_configuration",
    });
  });
});

describe("state mapping (02 §3.4)", () => {
  const base: BoatSandbox = { id: "s1", state: "ready", createdAt: "2026-11-22T10:00:00Z" };
  it.each<[string, Partial<BoatSandbox>, boolean, string, string | undefined]>([
    ["cancelled", { state: "cancelled" }, false, "exited", "unschedulable"],
    ["provisioning", { state: "provisioning" }, false, "starting", undefined],
    ["cloning", { state: "cloning" }, false, "starting", undefined],
    ["setup running", { setupStatus: "running" }, false, "running", undefined],
    ["setup pending, idle", { state: "idle", setupStatus: "pending" }, false, "running", undefined],
    ["setup done", { setupStatus: "done" }, false, "exited", "completed"],
    [
      "exit 75",
      { setupStatus: "failed", setupError: "script exited with code 75" },
      false,
      "exited",
      "network_policy_timeout",
    ],
    ["exit 64", { setupStatus: "failed", setupError: "exit status 64" }, false, "exited", "invalid_spec"],
    ["exit 65", { setupStatus: "failed", setupError: "exit status 65" }, false, "exited", "invalid_spec"],
    ["exit 1", { setupStatus: "failed", setupError: "exit status 1" }, false, "exited", "process_failed"],
    ["failed, no code", { setupStatus: "failed", setupError: "boom" }, false, "exited", "process_failed"],
    ["rebooted", { setupStatus: "running", lastReboot: { at: "2026-11-22T11:00:00Z" } }, false, "exited", "node_lost"],
    ["archived after cancel", { state: "archived" }, true, "exited", "cancelled"],
    [
      "archived by ttl",
      { state: "archived", archiveAfter: "2026-11-22T12:00:00Z", stoppedAt: "2026-11-22T12:00:01Z" },
      false,
      "exited",
      "deadline_exceeded",
    ],
    ["archived, other", { state: "archiving", stoppedAt: "2026-11-22T11:00:00Z" }, false, "exited", "evicted"],
    ["error", { state: "error", error: "host failed" }, false, "exited", "node_lost"],
  ])("%s", (_n, over, cancel, state, reason) => {
    const s = mapBoatSandbox("t.1", { ...base, ...over }, { cancelRequested: cancel });
    expect(s.state).toBe(state);
    expect(s.reason).toBe(reason);
  });

  it("adds the degraded health reason as the message", () => {
    const s = mapBoatSandbox("t.1", {
      ...base,
      setupStatus: "running",
      health: "degraded",
      healthReason: "agent unreachable",
    });
    expect(s).toMatchObject({ state: "running", message: "agent unreachable" });
  });

  it("reads exit codes from setupError", () => {
    expect(exitCodeFromSetupError("Exited with status 75")).toBe(75);
    expect(exitCodeFromSetupError("exit code: 2")).toBe(2);
    expect(exitCodeFromSetupError(null)).toBeUndefined();
  });
});

describe("cancel, destroy and list", () => {
  it("cancel runs kill as root and marks the task", async () => {
    const { f, p } = provider();
    const t = sampleTask();
    await p.start(t);
    await p.cancel(t.taskId, 30);
    const [c] = f.mock.find("POST", "/sandboxes/:id/commands");
    expect(JSON.parse(c!.body)).toEqual({
      command: "sudo -n kill -TERM $(cat /run/openfactory/runner.pid)",
      timeoutSeconds: 15,
    });
    f.sandboxes.get("sbx_1")!.state = "archived";
    expect((await p.status(t.taskId)).reason).toBe("cancelled");
  });

  it("destroy sends the confirm header and treats 404 as done", async () => {
    const { f, p } = provider();
    const t = sampleTask();
    await p.start(t);
    await p.destroy(t.taskId);
    expect(f.mock.find("DELETE", "/sandboxes/:id")[0]!.headers["x-ascii-confirm-delete"]).toBe("sbx_1");
    await p.destroy(t.taskId);
  });

  it("list follows pageInfo.nextCursor and restores task IDs", async () => {
    const f = fakeBoat();
    f.pageSize = 2;
    const { p } = provider(f);
    const ids: string[] = [];
    for (let i = 0; i < 5; i++) {
      const t = sampleTask();
      ids.push(t.taskId);
      await p.start(t);
    }
    // A sandbox of another install is ignored.
    f.sandboxes.set("other", {
      id: "other",
      state: "ready",
      labels: { "of-install": "x", "of-task": "run-a-1" },
      body: {},
    });
    const listed = await p.list();
    expect(listed.map((s) => s.taskId).sort()).toEqual(ids.sort());
    expect(f.mock.find("GET", "/sandboxes").length).toBe(3);
  });
});
