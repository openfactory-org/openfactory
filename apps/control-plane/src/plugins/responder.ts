// Adapts a responder plugin (Slack-like work sources) to the control plane
// responder shape, converting between the camelCase plugin API and the core wire
// types. Port of the responder part of internal/plugin/adapters.go.

import type { OutboundMessage, PullRequestRef, SourceRef } from "@openfactory/core";
import type { IssueState, ResponderPlugin } from "@openfactory/plugin-api";
import { outboundToPlugin, pullRequestToPlugin, sourceToPlugin } from "./mappers";

/** The responder port of the control plane (same shape as forges/registry Responder). */
export interface ResponderPort {
  provider(): string;
  reply(teamId: string, msg: OutboundMessage): Promise<void>;
  setIssueState?(teamId: string, issue: SourceRef, state: string, factoryId?: string): Promise<void>;
  attachPullRequest?(teamId: string, issue: SourceRef, pr: PullRequestRef, factoryId?: string): Promise<void>;
}

/** Serves one provider of a responder plugin. */
export class PluginResponder implements ResponderPort {
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
