// The router.status projection of a work item: its keys, and the source key
// that RouterDO needs to rebuild a route of kind 'source'.
import { describe, expect, it } from "vitest";
import type { EnqueueInput } from "../../lib/outbox/outbox";
import type { WorkItem, WorkItemRef } from "../../engine/types";
import type { Fx } from "./fx";
import { projectWorkItem } from "./projections";

function capture(): { fx: Fx; sent: EnqueueInput[] } {
  const sent: EnqueueInput[] = [];
  const fx = {
    teamId: "default",
    doName: "work-do",
    enqueue: (input: EnqueueInput) => {
      sent.push(input);
      return String(sent.length);
    },
  } as unknown as Fx;
  return { fx, sent };
}

const base: WorkItem = {
  id: "wi_1",
  factory_id: "fac_1",
  title: "t",
  stage: "building",
  source: { provider: "", kind: "" },
  created_at: "2026-01-01T00:00:00.000Z",
  updated_at: "2026-01-01T00:00:00.000Z",
  version: 3,
};
const issueRef: WorkItemRef = { kind: "issue", key: "jira:X-1", source: { provider: "jira", kind: "issue" } };

describe("projectWorkItem router.status", () => {
  it.each([
    {
      name: "with a source key",
      wi: { ...base, source_key: "github:o/r#1", references: [issueRef] },
      keys: ["github:o/r#1", "jira:X-1"],
      sourceKey: "github:o/r#1",
    },
    {
      name: "without a source key",
      wi: { ...base, references: [issueRef] },
      keys: ["jira:X-1"],
      sourceKey: undefined,
    },
  ])("$name", ({ wi, keys, sourceKey }) => {
    const { fx, sent } = capture();
    projectWorkItem(fx, wi);
    const status = sent.find((m) => m.kind === "router.status");
    expect(status?.msgId).toBe("rstat:wi_1:3");
    const payload = status?.payload as Record<string, unknown>;
    expect(payload["keys"]).toEqual(keys);
    if (sourceKey) expect(payload["sourceKey"]).toBe(sourceKey);
    else expect(payload).not.toHaveProperty("sourceKey");
  });
});
