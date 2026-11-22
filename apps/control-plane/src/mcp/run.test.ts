import { describe, expect, it } from "vitest";
import type { Env } from "../contracts/env";
import { defaultRunBackend, type RunView } from "./run";

const run: RunView = { id: "run_1", teamId: "team_1", factoryId: "fac_1", metadata: {}, mcpServers: {} };

function fakeEnv(secrets: Record<string, string>) {
  const calls: Array<{ factoryId: string; runId?: string; names: string[] }> = [];
  const env = {
    TEAM: {
      getByName: () => ({
        secretsResolve: async (_meta: unknown, input: { factoryId: string; runId?: string; names: string[] }) => {
          calls.push(input);
          return Object.fromEntries(input.names.filter((n) => n in secrets).map((n) => [n, secrets[n]!]));
        },
      }),
    },
  } as unknown as Env;
  return { env, calls };
}

describe("defaultRunBackend.expandSecrets", () => {
  it("reads every name in one call and expands with the secret store grammar", async () => {
    const { env, calls } = fakeEnv({ API_TOKEN: "t1", api_key: "k1" });
    const out = await defaultRunBackend.expandSecrets(env, run, "Bearer {{ API_TOKEN }} {{api_key}} {{API_TOKEN}}");
    // Lower-case names expand too (go-final used secrets.ExpandWith in the proxy).
    expect(out).toBe("Bearer t1 k1 t1");
    expect(calls).toEqual([{ factoryId: "fac_1", runId: "run_1", names: ["API_TOKEN", "api_key"] }]);
  });

  it("does not read secrets for text without placeholders", async () => {
    const { env, calls } = fakeEnv({});
    expect(await defaultRunBackend.expandSecrets(env, run, "{{a.b}} plain")).toBe("{{a.b}} plain");
    expect(calls).toEqual([]);
  });

  it("reports every missing name as resource_not_found", async () => {
    const { env } = fakeEnv({ A: "1" });
    await expect(defaultRunBackend.expandSecrets(env, run, "{{A}} {{B}} {{c}}")).rejects.toMatchObject({
      code: "resource_not_found",
      detail: "unknown secret(s) referenced: B, c",
    });
  });
});
