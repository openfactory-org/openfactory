// Later DO migrations drop tables and columns that no code reads or writes.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, newToken } from "@openfactory/core";
import { describe, expect, it } from "vitest";

function schema(sql: SqlStorage): { tables: string[]; columns: (t: string) => string[] } {
  const tables = sql
    .exec<{ name: string }>("SELECT name FROM sqlite_master WHERE type = 'table'")
    .toArray()
    .map((r) => r.name);
  const columns = (t: string) =>
    sql
      .exec<{ name: string }>(`PRAGMA table_info(${t})`)
      .toArray()
      .map((r) => r.name);
  return { tables, columns };
}

describe("unused schema is dropped", () => {
  it.each([
    {
      name: "IngressDO",
      stub: () => env.INGRESS.getByName(doName.ingress("t_mig", "p")),
      table: "events",
      gone: { tables: ["action_targets"], columns: ["payload_r2"] },
      kept: "payload",
    },
    {
      name: "DispatchDO",
      stub: () => env.DISPATCH.getByName(doName.dispatch("t_mig")),
      table: "waiting",
      gone: { tables: [], columns: ["hold_key"] },
      kept: "hold",
    },
    {
      name: "WorkDO",
      stub: () => env.WORK.getByName(doName.work("t_mig", newToken("w"))),
      table: "run_state",
      gone: { tables: ["work_item_state", "work_item_refs", "run_uploads"], columns: [] },
      kept: "v",
    },
  ])("$name", async ({ stub, table, gone, kept }) => {
    await runInDurableObject(stub(), (_i, state) => {
      const s = schema(state.storage.sql);
      for (const t of gone.tables) expect(s.tables).not.toContain(t);
      const cols = s.columns(table);
      expect(cols).toContain(kept);
      for (const c of gone.columns) expect(cols).not.toContain(c);
    });
  });
});
