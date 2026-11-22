// Port of internal/api/attachments_test.go: POST /attachments and the
// attachments of a run message. Also the environment check of POST /runs
// (internal/api/runs.go: "environment %s does not exist" is invalid_request).
import { env, exports } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import { BASE, bootstrap, call } from "./helpers";
import { MAX_ATTACHMENT_BYTES } from "../src/runner-api/transfer";

const uniq = () => crypto.randomUUID().slice(0, 6);

async function upload(
  query: string,
  key: string,
  body: Uint8Array | string,
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- tests read fields of the response
): Promise<{ status: number; body: any; raw: string }> {
  const headers: Record<string, string> = { "content-type": "text/plain" };
  if (key) headers["authorization"] = `Bearer ${key}`;
  const res = await exports.default.fetch(`${BASE}/api/v1/attachments${query}`, { method: "POST", headers, body });
  const raw = await res.text();
  let parsed: unknown = null;
  try {
    parsed = raw ? JSON.parse(raw) : null;
  } catch {
    /* not JSON */
  }
  return { status: res.status, body: parsed, raw };
}

async function newRun(key: string): Promise<string> {
  const f = (await call("POST", "/api/v1/factories", { key, body: { name: "Files " + uniq() } })).body.factory;
  const r = await call("POST", "/api/v1/runs", {
    key,
    body: { factory_id: f.id, agent_name: "foreman", prompt: "hi" },
  });
  expect(r.status, JSON.stringify(r.body)).toBe(201);
  return r.body.run_id as string;
}

describe("attachments (internal/api attachments_test ports)", () => {
  it("uploads an attachment: base name, content type, size, hidden object key", async () => {
    const { key } = await bootstrap();
    const r = await upload("?name=" + "..%5Cdir%2Fnotes.txt", key, "hello");
    expect(r.status, r.raw).toBe(201);
    expect(r.body.name).toBe("notes.txt");
    expect(r.body.content_type).toBe("text/plain");
    expect(r.body.bytes).toBe(5);
    const id = r.body.attachment_id as string;
    expect(id).toMatch(/^att_/);
    expect(r.body.key).toBeUndefined();
    expect(r.raw).not.toContain("attachments/");
    const obj = await env.DATA.get(`attachments/default/${id}/notes.txt`);
    expect(await obj?.text()).toBe("hello");

    expect((await upload("", key, "x")).status).toBe(400);
    expect((await upload("?name=..", key, "x")).status).toBe(400);
    const big = await upload("?name=big.bin", key, new Uint8Array(MAX_ATTACHMENT_BYTES + 1));
    expect(big.status, big.raw).toBe(413);
    expect(big.body.code).toBe("payload_too_large");
    expect((await upload("?name=a", "", "x")).status).toBe(401);
  });

  it("checks the attachments of a run message", async () => {
    const { key } = await bootstrap();
    const runId = await newRun(key);
    const path = `/api/v1/runs/${runId}/followups`;
    const missing = await call("POST", path, { key, body: { message: "see file", attachments: ["att_missing"] } });
    expect(missing.status, JSON.stringify(missing.body)).toBe(400);

    const id = (await upload("?name=a.txt", key, "a")).body.attachment_id as string;
    // An attachment of another team is not found in this team.
    await env.DATA.put("attachments/other-team/att_othr1/b.txt", "b");
    expect((await call("POST", path, { key, body: { message: "x", attachments: ["att_othr1"] } })).status).toBe(400);
    const tooMany = Array.from({ length: 21 }, () => id);
    expect((await call("POST", path, { key, body: { message: "x", attachments: tooMany } })).status).toBe(400);

    const ok = await call("POST", path, { key, body: { message: "see file", attachments: [id] } });
    expect(ok.status, JSON.stringify(ok.body)).toBe(202);
    const msgs = await call("GET", `/api/v1/runs/${runId}/messages`, { key });
    const last = (msgs.body.messages as { content: string; data?: { attachments?: string[] } }[]).at(-1)!;
    expect(last.content).toBe("see file");
    expect(last.data?.attachments).toEqual([id]);
  });

  it("refuses POST /runs with an environment that does not exist", async () => {
    const { key } = await bootstrap();
    const f = (await call("POST", "/api/v1/factories", { key, body: { name: "EnvRun " + uniq() } })).body.factory;
    const body = (environment_id: string) => ({
      factory_id: f.id,
      agent_name: "foreman",
      prompt: "hi",
      config: { environment_id },
    });
    const r = await call("POST", "/api/v1/runs", { key, body: body("env_missing") });
    expect(r.status, JSON.stringify(r.body)).toBe(400);
    expect(r.body.detail).toBe("environment env_missing does not exist");
    const e = await call("POST", "/api/v1/environments", { key, body: { name: "run-env-" + uniq() } });
    const ok = await call("POST", "/api/v1/runs", { key, body: body(e.body.id) });
    expect(ok.status, JSON.stringify(ok.body)).toBe(201);
  });
});
