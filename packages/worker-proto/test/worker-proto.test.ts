import { describe, expect, it } from "vitest";
import {
  authChallengeFrame,
  authMessage,
  base64url,
  decodeFrame,
  encodeFrame,
  errorFrame,
  FrameError,
  fromBase64url,
  heartbeatFrame,
  helloFrame,
  isFrame,
  isFrameType,
  PING_FRAME,
  requestFrame,
  responseFrame,
  taskStateFrame,
  validateFrame,
  validateWorkerDef,
  verifyAuthSignature,
  WORKER_ERROR_CODES,
  WORKER_SCHEMA,
  type Task,
} from "../src/index";

const task: Task = {
  task_id: "run_abc.1",
  run_id: "run_abc",
  session: 1,
  team_id: "default",
  image: "ghcr.io/example/sandbox:1",
  platform: { os: "linux", arch: "amd64" },
  resources: { cpu: "2", memory: "4Gi" },
  env: {
    OF_CONTROL_URL: "https://of.example.com",
    OF_RUN_ID: "run_abc",
    OF_SESSION: "1",
    OF_BOOTSTRAP_TOKEN: "ofb_test",
  },
  network_policy: { mode: "restricted", allowed_hosts: ["github.com"], denied_hosts: null },
  max_session_seconds: 3600,
  failure_retention_seconds: 0,
};

describe("frames", () => {
  it("round-trips valid frames", () => {
    const frames = [
      requestFrame("1", "task.assign", { task }),
      requestFrame("2", "task.status", { task_id: task.task_id }),
      requestFrame("3", "task.cancel", { task_id: task.task_id, grace_seconds: 10 }),
      requestFrame("4", "task.list", {}),
      requestFrame("5", "drain", { draining: true }),
      responseFrame("1", { status: { task_id: task.task_id, state: "accepted" } }),
      responseFrame("4", { tasks: [] }),
      errorFrame("6", "at_capacity", "full"),
      taskStateFrame({ task_id: task.task_id, state: "exited", reason: "completed", exit_code: 0 }),
      helloFrame({ worker_id: "wrk_1", backend: "microvm", capacity: 2, tasks: [] }),
      heartbeatFrame(1, 2),
      PING_FRAME,
    ];
    for (const f of frames) {
      const r = validateFrame(f);
      expect(r.errors, JSON.stringify(f)).toEqual([]);
      expect(decodeFrame(encodeFrame(f))).toEqual(f);
    }
  });

  it("rejects bad frames with locations", () => {
    expect(isFrame({ type: "request", id: "1", method: "task.assign", params: {} })).toBe(false);
    expect(isFrame({ type: "response", id: "1", result: {}, error: { code: "internal", message: "x" } })).toBe(false);
    expect(isFrame({ type: "nope" })).toBe(false);
    expect(() => decodeFrame("{")).toThrow(FrameError);
    try {
      decodeFrame(JSON.stringify({ type: "heartbeat", params: { active: "x", capacity: 1 } }));
      expect.unreachable();
    } catch (e) {
      expect(e).toBeInstanceOf(FrameError);
      expect((e as FrameError).errors.length).toBeGreaterThan(0);
    }
    expect(validateWorkerDef("task", { ...task, platform: { os: "darwin", arch: "amd64" } }).valid).toBe(false);
    expect(validateWorkerDef("task", task).valid).toBe(true);
  });

  it("narrows by type and lists the error codes of the schema", () => {
    const f = decodeFrame(encodeFrame(heartbeatFrame(0, 1)));
    expect(isFrameType(f, "heartbeat") && f.params.capacity).toBe(1);
    const codes = WORKER_SCHEMA.$defs.responseFrame.properties.error.properties.code.enum;
    expect([...WORKER_ERROR_CODES].sort()).toEqual([...codes].sort());
  });
});

describe("auth", () => {
  it("builds challenges and verifies Ed25519 signatures", async () => {
    const ch = authChallengeFrame("conn_1", [0]);
    expect(validateFrame(ch).errors).toEqual([]);
    expect(fromBase64url(ch.params.nonce)).toHaveLength(32);
    const kp = (await crypto.subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"])) as CryptoKeyPair;
    const pub = new Uint8Array((await crypto.subtle.exportKey("raw", kp.publicKey)) as ArrayBuffer);
    const sig = base64url(
      new Uint8Array(
        await crypto.subtle.sign({ name: "Ed25519" }, kp.privateKey, authMessage("wrk_1", "conn_1", ch.params.nonce)),
      ),
    );
    const resp = { type: "auth.response", params: { worker_id: "wrk_1", alg: "ed25519", sig } };
    expect(validateFrame(resp).errors).toEqual([]);
    expect(await verifyAuthSignature(pub, "wrk_1", "conn_1", ch.params.nonce, sig)).toBe(true);
    expect(await verifyAuthSignature(pub, "wrk_2", "conn_1", ch.params.nonce, sig)).toBe(false);
    expect(await verifyAuthSignature(pub, "wrk_1", "conn_1", ch.params.nonce, "AAAA")).toBe(false);
    expect(new TextDecoder().decode(authMessage("w", "c", "n"))).toBe("openfactory-worker-auth-v1\nw\nc\nn\n");
  });
});
