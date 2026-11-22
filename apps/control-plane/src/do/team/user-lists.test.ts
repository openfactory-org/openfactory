// TeamDO user lists: listUsers and listServiceAccounts filter by kind in SQL
// and give the same rows, in the same order, as a filter over all users.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CallMeta, Principal } from "../../contracts/do-messages";
import { listUsers, USER_KIND_SERVICE_ACCOUNT } from "./auth";
import type { TeamKit } from "./kit";

// eslint-disable-next-line @typescript-eslint/no-explicit-any -- loose JSON of the op results
type Loose = any;

describe("TeamDO user lists", () => {
  it("lists people and service accounts apart, oldest first, as the in-memory filter did", async () => {
    const id = "ul" + crypto.randomUUID().slice(0, 8);
    const admin: Principal = {
      kind: "user",
      id: "usr_admin",
      teamId: id,
      role: "admin",
      factoryRoles: {},
      userId: "usr_admin",
    };
    const meta: CallMeta = { requestId: "req_test", principal: admin };
    const stub = env.TEAM.getByName(doName.team(id));
    const op = async (name: string, body: unknown = null): Promise<Loose> => {
      const out = (await stub.api(meta, name, {}, body)) as Loose;
      const b = out && typeof out === "object" && "$status" in out ? out.body : out;
      if (b && typeof b === "object" && "$error" in b) throw new Error(`${name}: ${JSON.stringify(b.$error)}`);
      return b;
    };
    for (let i = 0; i < 4; i++) {
      await op("createUser", { email: `p${i}@example.com`, role: "user" });
      await op("createServiceAccount", { name: `sa${i}`, role: "editor" });
    }
    const people = (await op("listUsers")).items.map((u: { id: string }) => u.id);
    const accounts = (await op("listServiceAccounts")).items.map((u: { id: string }) => u.id);
    const all = await runInDurableObject(stub, async (instance) =>
      listUsers((instance as unknown as { kit: TeamKit }).kit),
    );
    const isSa = (u: { kind?: string }) => u.kind === USER_KIND_SERVICE_ACCOUNT;
    expect(people).toEqual(all.filter((u) => !isSa(u)).map((u) => u.id));
    expect(accounts).toEqual(all.filter(isSa).map((u) => u.id));
    expect(people.length).toBeGreaterThanOrEqual(4);
    expect(accounts).toHaveLength(4);
  });
});
