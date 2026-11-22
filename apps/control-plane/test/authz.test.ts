// Authorization of factory-scoped routes: the caller's role for the factory
// of the work item, run or path ({uid} as an ID, alias or name).
import { describe, expect, it } from "vitest";
import { bootstrap, call, email } from "./helpers";

const uniq = () => crypto.randomUUID().slice(0, 6);

async function factoryWithRun(
  key: string,
  name: string,
): Promise<{ id: string; alias: string; runId: string; wiId: string }> {
  const f = (await call("POST", "/api/v1/factories", { key, body: { name } })).body.factory;
  const r = await call("POST", `/api/v1/factories/${f.id}/runs`, { key, body: { prompt: "Do the work." } });
  expect(r.status, JSON.stringify(r.body)).toBe(201);
  return { id: f.id, alias: f.alias, runId: r.body.id, wiId: r.body.work_item_id };
}

async function eventually<T>(fn: () => Promise<T>, ok: (v: T) => boolean): Promise<T> {
  let v = await fn();
  for (let i = 0; i < 50 && !ok(v); i++) {
    await new Promise((r) => setTimeout(r, 20));
    v = await fn();
  }
  return v;
}

describe("factory-scoped routes", () => {
  it("check the caller's role for the factory of the work item, run and path", async () => {
    const { key } = await bootstrap();
    const a = await factoryWithRun(key, "Authz A " + uniq());
    const b = await factoryWithRun(key, "Authz B " + uniq());
    const u = await call("POST", "/api/v1/users", {
      key,
      body: { email: email("authz"), role: "user", create_api_key: true },
    });
    expect(u.status, JSON.stringify(u.body)).toBe(201);
    const userKey = u.body.key as string;
    await call("PATCH", `/api/v1/users/${u.body.user.id}`, { key, body: { factory_roles: { [a.id]: "editor" } } });

    const stage = (wiId: string) =>
      call("POST", `/api/v1/work-items/${wiId}/stage`, { key: userKey, body: { stage: "planning" } });
    expect((await stage(a.wiId)).status).toBe(200);
    const denied = await stage(b.wiId);
    expect(denied.status).toBe(403);
    expect(denied.body.detail).toBe("this operation requires the editor role; your role for this factory is user");

    // The user role reads both factories' runs (every team member has at least the user role).
    expect((await call("GET", `/api/v1/runs/${b.runId}`, { key: userKey })).status).toBe(200);
    expect((await call("GET", `/api/v1/work-items/${b.wiId}`, { key: userKey })).status).toBe(200);

    const selfImprove = (uid: string) => call("POST", `/api/v1/factories/${uid}/self-improvement`, { key: userKey });
    expect((await selfImprove(a.id)).status).toBe(200);
    expect((await selfImprove(b.id)).status).toBe(403);
  });

  it("list work items and inbox items by factory alias", async () => {
    const { key } = await bootstrap();
    const a = await factoryWithRun(key, "Alias List " + uniq());
    const byAlias = await eventually(
      () => call("GET", `/api/v1/factories/${a.alias}/work-items`, { key }),
      (r) => r.status !== 200 || (r.body.items as unknown[]).length > 0,
    );
    expect(byAlias.status, JSON.stringify(byAlias.body)).toBe(200);
    expect(byAlias.body.items.map((w: { id: string }) => w.id)).toEqual([a.wiId]);
    expect((await call("GET", `/api/v1/factories/${a.alias}/inbox`, { key })).status).toBe(200);
    expect((await call("GET", `/api/v1/factories/no-such-factory/work-items`, { key })).status).toBe(404);
    expect((await call("GET", `/api/v1/factories/no-such-factory/inbox`, { key })).status).toBe(404);
  });
});
