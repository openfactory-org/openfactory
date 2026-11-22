// TeamDO factories: the configSnapshot cache belongs to one TeamDO instance
// and drops an entry when the factory record changes.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { configSnapshot } from "./factories";
import type { TeamKit } from "./kit";

const FACTORY_ID = "fac_snapshot_cache";

function insertFactory(kit: TeamKit, name: string): void {
  const f = { id: FACTORY_ID, team_id: "x", alias: "snap", name, created_at: "2026-01-01T00:00:00Z" };
  kit.tx(() =>
    kit.sql.exec(
      `INSERT INTO factories (id, alias, name, active_version_id, created_ms, data) VALUES (?, ?, ?, '', ?, ?)
       ON CONFLICT(id) DO UPDATE SET name = excluded.name, data = excluded.data`,
      f.id,
      f.alias,
      f.name,
      Date.now(),
      JSON.stringify(f),
    ),
  );
}

async function snapshotName(team: string, name: string): Promise<string> {
  return runInDurableObject(env.TEAM.getByName("team:" + team), async (instance) => {
    const kit = (instance as unknown as { kit: TeamKit }).kit;
    insertFactory(kit, name);
    const snap = (await configSnapshot(kit, FACTORY_ID)) as { factory: { name: string } };
    return snap.factory.name;
  });
}

describe("configSnapshot cache", () => {
  it("does not share snapshots between TeamDO instances", async () => {
    // The same factory ID and version in two teams: each team reads its own record.
    expect(await snapshotName("snap-a", "Team A factory")).toBe("Team A factory");
    expect(await snapshotName("snap-b", "Team B factory")).toBe("Team B factory");
  });
});
