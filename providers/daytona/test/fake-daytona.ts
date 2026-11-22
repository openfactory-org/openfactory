// An in-memory Daytona API and toolbox on FetchMocks, shaped after the
// documented endpoints (https://www.daytona.io/docs/openapi.json). Fake values.
import { FetchMock } from "@openfactory/sandbox/testing";
import type { DaytonaSandbox } from "../src/index";

export const API = "https://daytona.example.test/api";
export const TOOLBOX = "https://proxy.daytona.example.test/toolbox";
export const FAKE_KEY = "dtn_test_not_a_real_key";

export interface FakeDaytona {
  api: FetchMock;
  toolbox: FetchMock;
  fetch: typeof fetch;
  sandboxes: Map<string, DaytonaSandbox & { body: Record<string, unknown> }>;
  exitCodes: Map<string, number>;
  snapshots: Map<string, { id: string; state: string }>;
  executed: { id: string; body: unknown }[];
}

export function fakeDaytona(): FakeDaytona {
  const api = new FetchMock(API);
  const toolbox = new FetchMock(TOOLBOX);
  const s: FakeDaytona = {
    api,
    toolbox,
    fetch: (input, init) => {
      const url = new Request(input, init).url;
      return url.startsWith(TOOLBOX) ? toolbox.fetch(input, init) : api.fetch(input, init);
    },
    sandboxes: new Map(),
    exitCodes: new Map(),
    snapshots: new Map([["of-sandbox-test", { id: "snap1", state: "active" }]]),
    executed: [],
  };
  let n = 0;
  const byIdOrName = (k: string) => s.sandboxes.get(k) ?? [...s.sandboxes.values()].find((x) => x.id === k);

  api.on("GET", "/snapshots/:name", ({ params }) => {
    const snap = s.snapshots.get(params.name!);
    return snap
      ? FetchMock.json({ ...snap, name: params.name })
      : FetchMock.json({ statusCode: 404, message: "Snapshot not found" }, 404);
  });
  api.on("POST", "/snapshots/:id/activate", ({ params }) => {
    for (const v of s.snapshots.values()) if (v.id === params.id) v.state = "pending";
    return FetchMock.json({ id: params.id, state: "pending" });
  });
  api.on("POST", "/snapshots", (req) => {
    const b = req.json<{ name: string }>();
    s.snapshots.set(b.name, { id: `snap-${b.name}`, state: "pending" });
    return FetchMock.json({ id: `snap-${b.name}`, state: "pending" }, 201);
  });
  api.on("POST", "/sandbox", (req) => {
    if (req.headers.get("authorization") !== `Bearer ${FAKE_KEY}`)
      return FetchMock.json({ statusCode: 401, message: "Unauthorized" }, 401);
    const body = req.json<Record<string, unknown>>();
    const name = body.name as string;
    if (s.sandboxes.has(name))
      return FetchMock.json({ statusCode: 409, message: `Sandbox with name ${name} already exists` }, 409);
    n += 1;
    const sb = {
      id: `00000000-0000-0000-0000-00000000000${n}`,
      name,
      state: "creating",
      createdAt: "2026-11-22T10:00:00Z",
      labels: body.labels as Record<string, string>,
      toolboxProxyUrl: TOOLBOX,
      body,
    };
    s.sandboxes.set(name, sb);
    return FetchMock.json(sb, 200, { "x-ratelimit-remaining-sandbox-create": "499" });
  });
  api.on("GET", "/sandbox/:name", ({ params }) => {
    const sb = byIdOrName(params.name!);
    return sb ? FetchMock.json(sb) : FetchMock.json({ statusCode: 404, message: "Sandbox not found" }, 404);
  });
  api.on("DELETE", "/sandbox/:name", ({ params }) => {
    const sb = byIdOrName(params.name!);
    if (!sb) return FetchMock.json({ statusCode: 404, message: "Sandbox not found" }, 404);
    s.sandboxes.delete(sb.name!);
    return FetchMock.empty(200);
  });
  api.on("PUT", "/sandbox/:name/labels", (req) => {
    const sb = byIdOrName(req.params.name!);
    if (!sb) return FetchMock.json({ statusCode: 404, message: "Sandbox not found" }, 404);
    sb.labels = req.json<{ labels: Record<string, string> }>().labels;
    return FetchMock.json({ labels: sb.labels });
  });
  api.on("GET", "/sandbox", ({ url }) => {
    const want = JSON.parse(url.searchParams.get("labels") ?? "{}") as Record<string, string>;
    return FetchMock.json(
      [...s.sandboxes.values()].filter((x) => Object.entries(want).every(([k, v]) => x.labels?.[k] === v)),
    );
  });
  toolbox.on("GET", "/:id/process/session/entrypoint", ({ params }) => {
    const code = s.exitCodes.get(params.id!);
    return FetchMock.json({
      sessionId: "entrypoint",
      commands: [{ id: "c1", command: "/usr/bin/tini", exitCode: code ?? null }],
    });
  });
  toolbox.on("POST", "/:id/process/execute", (req) => {
    s.executed.push({ id: req.params.id!, body: req.json() });
    return FetchMock.json({ exitCode: 0, result: "" });
  });
  return s;
}
