// Reserved worker IDs and names (direct.ReservedWorkerIDs and
// ValidateWorkerID in internal/sandbox/direct/protocol.go): a worker cannot
// take the ID or name of a built-in worker host or sandbox provider.
import { env } from "cloudflare:workers";
import { doName } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CallMeta, Principal, TeamRpc } from "../../contracts/do-messages";
import { RESERVED_WORKER_IDS, validateWorkerId } from "./workers";

const ADMIN: Principal = { kind: "user", id: "usr_admin", teamId: "default", role: "admin", factoryRoles: {} };
const meta: CallMeta = { requestId: "req_test", principal: ADMIN };
const team = (): TeamRpc => env.TEAM.getByName(doName.team("default")) as unknown as TeamRpc;
// eslint-disable-next-line @typescript-eslint/no-explicit-any -- op results are loose JSON
const op = (name: string, params: Record<string, string>, body: unknown): Promise<any> =>
  team().api(meta, name, params, body);

describe("reserved worker IDs", () => {
  it("validates a worker ID like ValidateWorkerID", () => {
    const cases: { id: string; err?: string }[] = [
      { id: "build-01" },
      { id: "wkr_0123abcd" },
      { id: "Server.2" },
      { id: "", err: "must be 1-64 characters" },
      { id: "-x", err: "must be 1-64 characters" },
      { id: "a".repeat(65), err: "must be 1-64 characters" },
      { id: "openfactory-1", err: 'worker ids starting with "openfactory" are reserved' },
      { id: "OpenFactory", err: 'worker ids starting with "openfactory" are reserved' },
      ...RESERVED_WORKER_IDS.map((id) => ({ id, err: `worker id "${id}" is reserved` })),
      { id: "SERVER", err: 'worker id "SERVER" is reserved' },
      { id: "Cloudflare-Sandbox", err: 'worker id "Cloudflare-Sandbox" is reserved' },
    ];
    for (const c of cases) {
      if (!c.err) expect(() => validateWorkerId(c.id), c.id).not.toThrow();
      else expect(() => validateWorkerId(c.id), c.id).toThrow(c.err);
    }
    expect([...RESERVED_WORKER_IDS].sort()).toEqual(
      ["boatdev", "cloudflare-sandbox", "daytona", "direct", "local", "managed", "server"].sort(),
    );
  });

  it("refuses a development worker with a reserved ID", async () => {
    for (const id of ["server", "Local", "daytona", "openfactory-dev"]) {
      const r = await op("workers.devRegister", { id }, null);
      expect(r.$error?.code, id).toBe("invalid_request");
    }
  });

  it("refuses a reserved worker name on enrollment", async () => {
    for (const name of ["server", "Managed", "boatdev", "openfactory"]) {
      const r = await op("createWorkerEnrollment", {}, { name });
      expect(r.$error?.code, name).toBe("invalid_request");
      expect(r.$error?.detail, name).toContain("is reserved");
    }
    const ok = await op("createWorkerEnrollment", {}, { name: "server-01" });
    expect(ok.$status, JSON.stringify(ok)).toBe(201);
  });
});
