import { describe, expect, it } from "vitest";
import {
  definitionVersionState,
  integrationWorkspace,
  isOfError,
  isTrackerIssue,
  normalizeAttachmentName,
  normalizeWorkspace,
  outboxPartitionKey,
  parseRunAs,
  personPushedBefore,
  roleFor,
  roleRank,
  runSourceAndRef,
  sameIntegrationProvider,
  sourceKey,
  startedByAutomation,
  TOPIC_INBOUND_EVENT,
  TOPIC_OUTBOUND_NOTICE,
  triggeredByFor,
  turnMarker,
  userPrincipal,
  workItemAddRef,
  workItemHasRef,
  workItemRefs,
  builtinMCPProvider,
  type SourceRef,
  type WorkItem,
} from "../src/index";

describe("work item references", () => {
  it("lists, adds and finds references", () => {
    const w = {
      source: { provider: "slack", kind: "thread", channel: "C1", thread_id: "1.2" },
      pull_requests: [{ forge: "GITHUB", repo: "acme/api", number: 4, url: "", branch: "", state: "" }],
      created_at: "2026-01-01T00:00:00Z",
    } as Pick<WorkItem, "source" | "references" | "pull_requests" | "created_at">;
    expect(
      workItemAddRef(w, { kind: "issue", key: "", source: { provider: "linear", kind: "", external_id: "ENG-1" } }),
    ).toBe(true);
    expect(
      workItemAddRef(w, { kind: "issue", key: "", source: { provider: "linear", kind: "", external_id: "ENG-1" } }),
    ).toBe(false);
    expect(workItemAddRef(w, { kind: "issue", key: "", source: { provider: "", kind: "" } })).toBe(false);
    const refs = workItemRefs(w);
    expect(refs.map((r) => [r.kind, r.key])).toEqual([
      ["source", "slack:C1:1.2"],
      ["issue", "linear:ENG-1"],
      ["pull_request", "github:acme/api#4"],
    ]);
    for (const k of ["slack:C1:1.2", "linear:ENG-1", "github:acme/api#4"]) expect(workItemHasRef(w, k)).toBe(true);
    expect(workItemHasRef(w, "")).toBe(false);
    expect(workItemHasRef(w, "github:acme/api#5")).toBe(false);
  });

  it("leaves the thread ref out of the key", () => {
    const a: SourceRef = { provider: "github", kind: "", repo: "acme/api", number: 4 };
    expect(sourceKey({ ...a, thread_ref: "99" })).toBe(sourceKey(a));
  });

  it.each([
    [{ provider: "linear", kind: "", external_id: "ENG-1" }, true],
    [{ provider: "jira", kind: "issue", external_id: "OPS-2" }, true],
    [{ provider: "azure_devops", kind: "work_item", repo: "proj", number: 3 }, true],
    [{ provider: "github", kind: "issue", repo: "a/b", number: 3 }, false],
    [{ provider: "slack", kind: "", channel: "C1" }, false],
  ] as [SourceRef, boolean][])("isTrackerIssue(%o)", (src, want) => {
    expect(isTrackerIssue(src)).toBe(want);
  });
});

describe("run sources", () => {
  it("prefers the stored source and falls back to triggered_by", () => {
    expect(runSourceAndRef({ triggered_by: "automation:a1", source: "SCHEDULE", source_ref: "a1" })).toEqual({
      source: "SCHEDULE",
      ref: "a1",
    });
    const fallback: Record<string, string> = {
      "automation:a1": "AUTOMATION",
      "schedule:a1": "SCHEDULE",
      "webhook:a1": "WEBHOOK",
      "run:r1": "ORCHESTRATION",
      mention: "MENTION",
      "": "API",
    };
    for (const [t, want] of Object.entries(fallback)) expect(runSourceAndRef({ triggered_by: t }).source).toBe(want);
  });

  it("builds and checks triggered_by", () => {
    expect(triggeredByFor("schedule", "a1")).toBe("schedule:a1");
    expect(triggeredByFor("webhook", "a1")).toBe("webhook:a1");
    expect(triggeredByFor("github", "a1")).toBe("automation:a1");
    for (const by of ["automation:a1", "schedule:a1", "webhook:a1"]) {
      expect(startedByAutomation(by, "a1")).toBe(true);
      expect(startedByAutomation(by, "a2")).toBe(false);
    }
    expect(startedByAutomation("api", "a1")).toBe(false);
    expect(startedByAutomation("automation:", "")).toBe(false);
  });

  it("marks turn ends", () => {
    expect(turnMarker({ consumed_seq: 3, turns: 2, state: "succeeded" })).toBe("3:2");
    expect(turnMarker({ consumed_seq: 3, state: "cancelled" })).toBe("3:0:cancelled");
  });
});

describe("outbox partition keys", () => {
  it("orders events of one source per team and notices per work item", () => {
    const pr: SourceRef = { provider: "forge", kind: "pull_request", repo: "acme/api", number: 7 };
    const k = outboxPartitionKey(TOPIC_INBOUND_EVENT, { kind: "event", event: { team_id: "team1", source: pr } });
    expect(k).not.toBe("");
    expect(
      outboxPartitionKey(TOPIC_INBOUND_EVENT, { kind: "event", event: { team_id: "team2", source: pr } }),
    ).not.toBe(k);
    expect(
      outboxPartitionKey(TOPIC_INBOUND_EVENT, {
        kind: "event",
        event: { team_id: "team1", source: { provider: "", kind: "" } },
      }),
    ).toBe("");
    const n = outboxPartitionKey(TOPIC_OUTBOUND_NOTICE, {
      kind: "notice",
      notice: { work_item_id: "wi1", source: pr },
    });
    expect(n).not.toBe("");
    expect(n).not.toBe(k);
    expect(
      outboxPartitionKey(TOPIC_OUTBOUND_NOTICE, { kind: "notice", notice: { work_item_id: "", source: pr } }),
    ).not.toBe("");
    expect(outboxPartitionKey("jobs", { kind: "key", key: "abc" })).toBe("jobs|abc");
    expect(outboxPartitionKey("jobs", { kind: "none" })).toBe("");
  });
});

describe("identity", () => {
  it("ranks roles and applies factory overrides", () => {
    expect(roleRank(" Admin ")).toBe(3);
    expect(roleRank("x")).toBe(0);
    const u = { role: "user", factory_roles: { f1: "EDITOR", f2: "bogus" } };
    expect(roleFor(u, "f1")).toBe("editor");
    expect(roleFor(u, "f2")).toBe("user");
    expect(roleFor(u)).toBe("user");
    expect(roleFor(null)).toBe("");
  });

  it("parses runAs", () => {
    expect(parseRunAs("")).toEqual({ type: "factory", value: "" });
    expect(parseRunAs("user: a@b.c")).toEqual({ type: "user", value: "a@b.c" });
    expect(parseRunAs("serviceAccount:bot")).toEqual({ type: "service_account", value: "bot" });
    for (const bad of ["user:nope", "serviceAccount:", "team:x"]) {
      try {
        parseRunAs(bad);
        expect.unreachable();
      } catch (e) {
        expect(isOfError(e, "invalid_request")).toBe(true);
      }
    }
  });

  it("builds principals", () => {
    expect(userPrincipal({ id: "sa_1", name: "", email: "x@y", kind: "service_account" })).toEqual({
      type: "service_account",
      id: "sa_1",
      name: "x@y",
    });
    expect(userPrincipal({ id: "", name: "", email: "" })).toBeNull();
  });
});

describe("integrations", () => {
  it("compares providers and normalizes workspaces", () => {
    expect(sameIntegrationProvider("Microsoft-Teams", "teams")).toBe(true);
    expect(builtinMCPProvider("builtin/linear")).toEqual({ provider: "linear", known: true });
    expect(builtinMCPProvider("builtin/x")).toEqual({ provider: "x", known: false });
    expect(normalizeWorkspace("jira", "https://Acme.atlassian.net/browse/X-1")).toBe("acme.atlassian.net");
    expect(normalizeWorkspace("jira", "https://api.atlassian.com/ex/jira/ABC-123/rest")).toBe("abc-123");
    expect(normalizeWorkspace("slack", " T1 ")).toBe("T1");
    expect(integrationWorkspace({ provider: "linear", config: { organization_id: "o1" } })).toBe("o1");
    expect(integrationWorkspace({ provider: "github", config: {} })).toBe("");
  });
});

describe("misc", () => {
  it("normalizes attachment names", () => {
    expect(normalizeAttachmentName(" C:\\docs\\a.txt ")).toBe("a.txt");
    expect(normalizeAttachmentName("dir/b.png")).toBe("b.png");
    for (const bad of ["", "..", "a/..", "x".repeat(256)]) expect(() => normalizeAttachmentName(bad)).toThrow();
  });

  it("derives definition version states", () => {
    expect(definitionVersionState({ id: "v1", valid: true, state: "proposed" }, "v1")).toBe("proposed");
    expect(definitionVersionState({ id: "v1", valid: false }, "v1")).toBe("invalid");
    expect(definitionVersionState({ id: "v1", valid: true }, "v1")).toBe("active");
    expect(definitionVersionState({ id: "v0", valid: true }, "v1")).toBe("superseded");
  });

  it("checks person pushes", () => {
    const f = { opener_kind: "agent", pushes: [{ at: "2026-01-02T00:00:00Z", kind: "person" }] };
    expect(personPushedBefore(f, "2026-01-01T00:00:00Z")).toBe(false);
    expect(personPushedBefore(f, "2026-01-02T00:00:00Z")).toBe(true);
  });
});
