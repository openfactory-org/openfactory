// Adapts a responder plugin (Slack-like work sources) to the control plane
// responder shape, converting between the camelCase plugin API and the core wire
// types. Port of the responder part of internal/plugin/adapters.go.

import type { OutboundMessage, PullRequestRef, Responder, SourceRef } from "@openfactory/core";
import type { IssueState, ResponderPlugin } from "@openfactory/plugin-api";
import { outboundToPlugin, pullRequestToPlugin, sourceToPlugin } from "./mappers";

/** Serves one provider of a responder plugin. */
export class PluginResponder implements Responder {
  constructor(
    private readonly plugin: ResponderPlugin,
    private readonly providerName: string,
  ) {}

  provider(): string {
    return this.providerName;
  }

  reply(teamId: string, msg: OutboundMessage): Promise<void> {
    return this.plugin.reply(this.providerName, teamId, outboundToPlugin(msg));
  }

  setIssueState(teamId: string, issue: SourceRef, state: string): Promise<void> {
    return this.plugin.setIssueState(this.providerName, teamId, sourceToPlugin(issue), state as IssueState);
  }

  attachPullRequest(teamId: string, issue: SourceRef, pr: PullRequestRef): Promise<void> {
    return this.plugin.attachPullRequest(this.providerName, teamId, sourceToPlugin(issue), pullRequestToPlugin(pr));
  }
}
