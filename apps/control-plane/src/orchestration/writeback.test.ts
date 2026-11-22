// Tracker write-back (internal/engine/references_test.go): a linked issue
// gets the pull requests of the work item and moves to in review, and to done
// when the work item completes. A work item that started on a tracker issue
// writes back to that issue; a GitHub issue source does not.
import { env } from "cloudflare:workers";
import type { OutboundMessage } from "@openfactory/core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { OutboxEnvelope } from "../contracts/do-messages";
import { completeWorkItem } from "../do/work/intake";
import { setNoticeDeliverer } from "../do/work/index";
import { deliverNotice } from "../notify/index";
import { createWorkItem } from "../do/work/engine";
import { buildRuns, FACTORY, resultText, settle, withFx, workStub } from "./fixture.test-util";
import { callTool } from "./index";

let sent: OutboundMessage[] = [];
beforeEach(() => {
  sent = [];
  setNoticeDeliverer(async (_env, e: OutboxEnvelope) => {
    if (e.kind === "notice.tracker") sent.push((e.payload as unknown as { message: OutboundMessage }).message);
  });
});
afterEach(async () => {
  await settle();
  setNoticeDeliverer((ev, e) => deliverNotice(ev, e));
});

async function delivered(token: string): Promise<OutboundMessage[]> {
  for (let i = 0; i < 3; i++) await workStub(token).pump();
  return sent;
}

describe("tracker write-back", () => {
  it("a linked issue follows the pull requests and the completion (TestLinkIssueWriteBack)", async () => {
    const f = await buildRuns(
      { wi: { stage: "building", root: "root" } },
      { root: { type: "FOREMAN", workItem: "wi", state: "running" } },
    );
    const link = { url: "https://linear.app/acme/issue/ENG-12" };
    for (let i = 0; i < 2; i++) {
      // A second link of the same issue changes nothing.
      const r = await callTool(env, "default", f.ids.root!, "link_issue", link);
      expect(r.isError, resultText(r)).toBeFalsy();
    }
    let ops = await delivered(f.token);
    expect(ops).toHaveLength(1);
    expect(ops[0]).toMatchObject({
      kind: "issue_state",
      issue_state: "in_progress",
      source: { external_id: "ENG-12" },
    });

    const pr = await callTool(env, "default", f.ids.root!, "link_pull_request", {
      url: "https://github.com/acme/api/pull/42",
    });
    expect(pr.isError, resultText(pr)).toBeFalsy();
    ops = (await delivered(f.token)).slice(1);
    expect(ops).toHaveLength(2);
    expect(ops[0]).toMatchObject({ kind: "attach_pull_request", pull_request: { number: 42 } });
    expect(ops[1]).toMatchObject({ kind: "issue_state", issue_state: "in_review" });

    // The completion moves the issue to done.
    const done = await callTool(env, "default", f.ids.root!, "complete_work_item", { note: "merged" });
    expect(done.isError, resultText(done)).toBeFalsy();
    ops = await delivered(f.token);
    expect(ops[ops.length - 1]).toMatchObject({
      kind: "issue_state",
      issue_state: "done",
      source: { external_id: "ENG-12" },
    });
    // A completed work item cannot get a new issue.
    const late = await callTool(env, "default", f.ids.root!, "link_issue", { provider: "jira", key: "OPS-1" });
    expect(late.isError).toBe(true);
  });

  it("a tracker source writes back; a GitHub issue source does not (TestTrackerSourceWriteBack)", async () => {
    const f = await buildRuns({}, {});
    const ids = await withFx(f.token, (fx) => {
      const jira = createWorkItem(
        fx,
        {
          factoryId: FACTORY,
          title: "jira",
          source: { provider: "jira", kind: "issue", external_id: "OPS-7" },
          sourceKey: "jira:OPS-7",
        },
        "usr_1",
      );
      const gh = createWorkItem(
        fx,
        {
          factoryId: FACTORY,
          title: "gh",
          source: { provider: "github", kind: "issue", repo: "acme/api", number: 3 },
          sourceKey: "github:acme/api#3",
        },
        "usr_1",
      );
      return { jira: jira.id, gh: gh.id };
    });
    await withFx(f.token, (fx) => completeWorkItem(fx, ids.jira, "system", ""));
    let ops = await delivered(f.token);
    expect(ops).toHaveLength(1);
    expect(ops[0]).toMatchObject({ source: { external_id: "OPS-7" }, issue_state: "done" });
    await withFx(f.token, (fx) => completeWorkItem(fx, ids.gh, "system", ""));
    ops = await delivered(f.token);
    expect(ops).toHaveLength(1);
  });
});
