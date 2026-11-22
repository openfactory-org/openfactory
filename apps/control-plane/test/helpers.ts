import { env, exports } from "cloudflare:workers";

export const BASE = "http://localhost";

/** Calls the Worker. Each test file gets fresh DO storage per test (isolated storage). */
export async function call(
  method: string,
  path: string,
  opts: { key?: string; body?: unknown; headers?: Record<string, string> } = {},
): Promise<{ status: number; body: any; res: Response }> {
  const headers: Record<string, string> = { ...(opts.headers ?? {}) };
  if (opts.key) headers["authorization"] = `Bearer ${opts.key}`;
  let body: string | undefined;
  if (opts.body !== undefined) {
    headers["content-type"] = "application/json";
    body = typeof opts.body === "string" ? opts.body : JSON.stringify(opts.body);
  }
  const res = await exports.default.fetch(BASE + path, { method, headers, ...(body !== undefined ? { body } : {}) });
  const text = await res.clone().text();
  let parsed: unknown = text;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    /* not JSON */
  }
  return { status: res.status, body: parsed, res };
}

let admin: Promise<{ key: string; userId: string }> | undefined;

const forged = (userId: string) => ({
  requestId: "test",
  principal: { kind: "user" as const, id: userId, teamId: "default", role: "admin" as const, factoryRoles: {}, userId },
});

/**
 * An admin API key of the default team. Storage is shared by the test files
 * of one run, so this bootstraps when needed and otherwise mints a key for
 * the existing admin through TeamDO.api.
 */
export function bootstrap(): Promise<{ key: string; userId: string }> {
  admin ??= (async () => {
    const r = await call("POST", "/api/v1/bootstrap", { body: { team_name: "Acme", admin_email: "admin@example.com" } });
    if (r.status === 201) return { key: r.body.key as string, userId: r.body.user.id as string };
    const team = env.TEAM.getByName("team:default");
    const users = (await team.api(forged("x"), "listUsers", {}, undefined)) as { items: { id: string; role: string }[] };
    const a = users.items.find((u) => u.role === "admin")!;
    const k = (await team.api(forged(a.id), "createAPIKey", {}, { name: "test" })) as { body: { key: string } };
    return { key: k.body.key, userId: a.id };
  })();
  return admin;
}

/** A unique email for this run. */
export const email = (p: string) => `${p}-${crypto.randomUUID().slice(0, 8)}@example.com`;
