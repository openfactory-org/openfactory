// TeamDO secrets: a create of an existing name is a conflict, also when two
// creates of the same name interleave.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, type OfError } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CallMeta, Principal, TeamRpc } from "../../contracts/do-messages";
import type { TeamKit } from "./kit";
import { getSecretValue, putSecret } from "./secrets";

const ADMIN: Principal = { kind: "user", id: "usr_admin", teamId: "secrets", role: "admin", factoryRoles: {} };
const meta: CallMeta = { requestId: "req_test", principal: ADMIN };
const team = (): TeamRpc => env.TEAM.getByName(doName.team("secrets")) as unknown as TeamRpc;
// eslint-disable-next-line @typescript-eslint/no-explicit-any -- op results are loose JSON
const op = (name: string, body: unknown): Promise<any> => team().api(meta, name, {}, body);

describe("TeamDO createSecret", () => {
  it("refuses an existing name", async () => {
    const first = await op("createSecret", { name: "ONCE_ONLY", value: "v1" });
    expect(first.$status, JSON.stringify(first)).toBe(201);
    const again = await op("createSecret", { name: "ONCE_ONLY", value: "v2" });
    expect(again.$error?.code).toBe("conflict");
    expect((await op("secretsGet", { name: "ONCE_ONLY" })).value).toBe("v1");
  });

  it("checks createOnly in the write transaction, after the value is sealed", async () => {
    // Two creates that both pass the early check and then await the seal:
    // the one that writes second must fail instead of overwriting.
    await runInDurableObject(env.TEAM.getByName(doName.team("secrets")), async (instance) => {
      const kit = (instance as unknown as { kit: TeamKit }).kit;
      const values = ["first-value", "second-value"];
      const out = await Promise.allSettled(
        values.map((v) => putSecret(kit, "", "RACED", "generic", v, { createOnly: true })),
      );
      expect(out.map((r) => r.status)).toEqual(["fulfilled", "rejected"]);
      const err = (out[1] as PromiseRejectedResult).reason as OfError;
      expect(err.code).toBe("conflict");
      expect(await getSecretValue(kit, "", "RACED")).toBe("first-value");
    });
  });
});
