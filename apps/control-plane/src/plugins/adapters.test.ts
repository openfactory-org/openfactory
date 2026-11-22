import { describe, expect, it } from "vitest";
import type { OutboundMessage } from "@openfactory/core";
import type { ResponderPlugin, SandboxPlugin } from "@openfactory/plugin-api";
import { outboundToPlugin, pullRequestFromPlugin, pullRequestToPlugin, sourceFromPlugin, sourceToPlugin } from "./mappers";
import { PluginResponder } from "./responder";
import { adaptSandboxPlugin } from "./sandbox";

const msg: OutboundMessage = {
  work_item_id: "wi_1",
  source: { provider: "slack", kind: "thread", channel: "C1", thread_id: "T1", thread_ref: "r", title: "t" },
  text: "hi",
  kind: "question",
  options: ["a"],
  factory_id: "fac_1",
  issue_state: "in_review",
  pull_request: { forge: "GITLAB", repo: "a/b", number: 3, url: "u", branch: "x", state: "open" },
};

describe("mappers", () => {
  it("converts an outbound message to camelCase", () => {
    expect(outboundToPlugin(msg)).toEqual({
      workItemId: "wi_1",
      source: { provider: "slack", kind: "thread", channel: "C1", threadId: "T1", threadRef: "r", title: "t" },
      text: "hi",
      kind: "question",
      options: ["a"],
      factoryId: "fac_1",
      issueState: "in_review",
      pullRequest: { forge: "GITLAB", repo: "a/b", number: 3, url: "u", branch: "x", state: "open" },
    });
  });

  it("round-trips a source and a pull request", () => {
    const s = msg.source;
    expect(sourceFromPlugin(sourceToPlugin(s))).toEqual(s);
    const pr = msg.pull_request!;
    expect(pullRequestFromPlugin(pullRequestToPlugin(pr))).toEqual(pr);
    expect(pullRequestFromPlugin({ forge: "F", repo: "r", number: 1 })).toEqual({ forge: "F", repo: "r", number: 1, url: "", branch: "", state: "" });
  });
});

describe("PluginResponder", () => {
  it("calls the plugin with the provider and the converted payload", async () => {
    const calls: unknown[][] = [];
    const plugin = {
      reply: async (...a: unknown[]) => void calls.push(["reply", ...a]),
      setIssueState: async (...a: unknown[]) => void calls.push(["state", ...a]),
      attachPullRequest: async (...a: unknown[]) => void calls.push(["attach", ...a]),
    } as unknown as ResponderPlugin;
    const r = new PluginResponder(plugin, "slack");
    expect(r.provider()).toBe("slack");
    await r.reply("team_1", msg);
    await r.setIssueState("team_1", msg.source, "done");
    await r.attachPullRequest("team_1", msg.source, msg.pull_request!);
    expect(calls[0]).toEqual(["reply", "slack", "team_1", outboundToPlugin(msg)]);
    expect(calls[1]).toEqual(["state", "slack", "team_1", sourceToPlugin(msg.source), "done"]);
    expect(calls[2]).toEqual(["attach", "slack", "team_1", sourceToPlugin(msg.source), pullRequestToPlugin(msg.pull_request!)]);
  });
});

describe("adaptSandboxPlugin", () => {
  it("forwards every method and takes the plugin name as the id", async () => {
    const calls: string[] = [];
    const plugin = Object.fromEntries(
      ["capabilities", "preflight", "start", "status", "cancel", "destroy", "list"].map((m) => [m, async (...a: unknown[]) => (calls.push(`${m}:${a.join(",")}`), m === "list" ? [] : undefined)]),
    ) as unknown as SandboxPlugin;
    const p = adaptSandboxPlugin("acme-sbx", plugin);
    expect(p.id).toBe("acme-sbx");
    await p.capabilities();
    await p.preflight({ taskId: "t1" } as never);
    await p.start({ taskId: "t1" } as never);
    await p.status("t1");
    await p.cancel("t1", 30);
    await p.destroy("t1");
    expect(await p.list()).toEqual([]);
    expect(calls).toEqual(["capabilities:", "preflight:[object Object]", "start:[object Object]", "status:t1", "cancel:t1,30", "destroy:t1", "list:"]);
  });
});
