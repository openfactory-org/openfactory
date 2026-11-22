// WorkDO.reopen: a completed work item goes back to triage with a "reopened"
// timeline entry and is projected again, so IndexDO and RouterDO see the new
// stage (internal/engine/references.go reopenWorkItem).
import { env } from "cloudflare:workers";
import { doName } from "@openfactory/core";
import { afterEach, describe, expect, it } from "vitest";
import type { CoreEvent } from "../../contracts/do-messages";
import { buildRuns, FACTORY, settle, TEAM, withFx, workStub } from "../../orchestration/fixture.test-util";

afterEach(settle);

const event: CoreEvent = {
  id: "evt_reopen",
  team_id: TEAM,
  provider: "github",
  type: "issue_commented",
  actor: { provider: "github", id: "1", login: "dana" },
  source: { provider: "github", kind: "issue", repo: "acme/api", number: 7 },
  attributes: null,
  received_at: "2026-11-22T00:00:00Z",
} as unknown as CoreEvent;

describe("WorkDO.reopen", () => {
  it("moves a completed work item to triage, records the timeline entry and projects it", async () => {
    const f = await buildRuns({ wi: { stage: "complete" } }, {});
    const wiId = f.ids.wi!;
    expect(await workStub(f.token).reopen({ actionKey: "ak_1", event })).toEqual({ ok: true });

    const after = await withFx(f.token, (fx) => ({
      wi: fx.repo.getWorkItem(wiId)!,
      reopened: fx.repo.listWorkItemEvents(wiId).filter((e) => e.kind === "reopened"),
    }));
    expect(after.wi.stage).toBe("triage");
    expect(after.wi.completed_at).toBeUndefined();
    expect(after.reopened).toHaveLength(1);
    expect(after.reopened[0]!.data).toMatchObject({ from: "complete", to: "triage", key: "github:acme/api#7" });

    // The projection reaches IndexDO: the summary is no longer "complete".
    const index = env.INDEX.getByName(doName.index(TEAM));
    const end = Date.now() + 3000;
    let stage = "";
    while (Date.now() < end) {
      await workStub(f.token).pump();
      const res = (await index.list({ requestId: "t" }, "workItems", { factory_id: FACTORY })) as {
        items?: { id: string; stage: string }[];
        data?: { id: string; stage: string }[];
      };
      stage = (res.items ?? res.data ?? []).find((w) => w.id === wiId)?.stage ?? "";
      if (stage === "triage") break;
      await new Promise((r) => setTimeout(r, 20));
    }
    expect(stage).toBe("triage");

    // The same action key does not reopen again.
    expect(await workStub(f.token).reopen({ actionKey: "ak_1", event })).toEqual({ ok: true });
    const events = await withFx(f.token, (fx) => fx.repo.listWorkItemEvents(wiId).filter((e) => e.kind === "reopened"));
    expect(events).toHaveLength(1);
  });
});
