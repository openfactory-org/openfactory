// An in-memory boat.dev API on a FetchMock, shaped after the documented
// endpoints (https://docs.boat.dev/api/v1). Clearly fake values.
import { FetchMock } from "@openfactory/sandbox/testing";
import type { BoatSandbox } from "../src/index";

export const FAKE_BASE = "https://boat.example.test/api/v1";
export const FAKE_KEY = "boat_test_not_a_real_key";

export interface FakeBoat {
  mock: FetchMock;
  sandboxes: Map<string, BoatSandbox & { body: Record<string, unknown> }>;
  keys: Map<string, { id: string; body: string }>;
  pageSize: number;
}

export function fakeBoat(): FakeBoat {
  const mock = new FetchMock(FAKE_BASE);
  const state: FakeBoat = { mock, sandboxes: new Map(), keys: new Map(), pageSize: 200 };
  let n = 0;
  const auth = (h: Headers) => h.get("authorization") === `Bearer ${FAKE_KEY}`;
  const deny = () =>
    FetchMock.json({ ok: false, code: "unauthorized", error: { code: "unauthorized", status: 401 } }, 401);

  mock.on("GET", "/me", ({ headers }) =>
    auth(headers) ? FetchMock.json({ ok: true, type: "user", user: { id: "u1" } }) : deny(),
  );
  mock.on("GET", "/limits", () =>
    FetchMock.json({ ok: true, type: "limits", canStart: true, machineTypes: ["small", "default", "large"] }),
  );
  mock.on("GET", "/saved-snapshots/:name", ({ params }) =>
    params.name === "missing"
      ? FetchMock.json(
          { ok: false, code: "snapshot_not_found", error: { code: "snapshot_not_found", status: 404 } },
          404,
        )
      : FetchMock.json({ ok: true, type: "saved_snapshot", snapshot: { name: params.name, status: "ready" } }),
  );
  mock.on("POST", "/sandboxes", (req) => {
    if (!auth(req.headers)) return deny();
    const key = req.headers.get("idempotency-key") ?? "";
    const prior = state.keys.get(key);
    if (prior) {
      if (prior.body !== req.body) {
        return FetchMock.json(
          { ok: false, code: "idempotency_key_reused", error: { code: "idempotency_key_reused", status: 409 } },
          409,
        );
      }
      return FetchMock.json({ ok: true, type: "sandbox", sandbox: state.sandboxes.get(prior.id) }, 200);
    }
    const body = req.json<Record<string, unknown>>();
    n += 1;
    const id = `sbx_${n}`;
    const sb = {
      id,
      name: body.name as string,
      state: "provisioning",
      setupStatus: "pending" as const,
      createdAt: new Date(Date.UTC(2026, 10, 22, 10, 0, n)).toISOString(),
      labels: body.labels as Record<string, string>,
      body,
    };
    state.sandboxes.set(id, sb);
    if (key) state.keys.set(key, { id, body: req.body });
    return FetchMock.json({ ok: true, type: "sandbox", sandbox: sb }, 201, {
      "ratelimit-policy": '"starts-minute";q=12;w=60',
      ratelimit: '"starts-minute";r=11;t=60',
    });
  });
  mock.on("GET", "/sandboxes", ({ url }) => {
    const labels = url.searchParams.getAll("label").map((l) => l.split("=") as [string, string]);
    const all = [...state.sandboxes.values()].filter((s) => labels.every(([k, v]) => s.labels?.[k] === v));
    const start = Number(url.searchParams.get("cursor") ?? "0");
    const page = all.slice(start, start + state.pageSize);
    const next = start + state.pageSize < all.length ? String(start + state.pageSize) : null;
    return FetchMock.json({ ok: true, type: "sandbox_list", sandboxes: page, pageInfo: { nextCursor: next } });
  });
  mock.on("GET", "/sandboxes/:id", ({ params }) => {
    const sb = state.sandboxes.get(params.id!);
    return sb
      ? FetchMock.json({ ok: true, type: "sandbox", sandbox: sb })
      : FetchMock.json({ ok: false, code: "not_found" }, 404);
  });
  mock.on("POST", "/sandboxes/:id/commands", ({ params }) =>
    state.sandboxes.has(params.id!)
      ? FetchMock.json({ ok: true, type: "command", status: "exited", exitCode: 0 })
      : FetchMock.json({ ok: false, code: "not_found" }, 404),
  );
  mock.on("DELETE", "/sandboxes/:id", ({ params, headers }) => {
    if (headers.get("x-ascii-confirm-delete") !== params.id) {
      return FetchMock.json({ ok: false, code: "confirmation_required" }, 400);
    }
    if (!state.sandboxes.delete(params.id!)) return FetchMock.json({ ok: false, code: "not_found" }, 404);
    return FetchMock.json({ ok: true, type: "deletion_operation", operation: { id: `del_${params.id}` } }, 202);
  });
  return state;
}
