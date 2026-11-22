// The responder port: posts outbound messages back to a work source. Port of
// core.Responder and core.IssueTracker. Forges, integrations and responder
// plugins implement it; the notifier calls it.
import type { OutboundMessage, PullRequestRef, SourceRef } from "./workitem";

/** Posts outbound messages back to a work source. Trackers also write the issue state back. */
export interface Responder {
  provider(): string;
  reply(teamId: string, msg: OutboundMessage): Promise<void>;
  /** Tracker write-back: moves the issue to state (core.IssueTracker). */
  setIssueState?(teamId: string, issue: SourceRef, state: string, factoryId?: string): Promise<void>;
  /** Tracker write-back: links the pull request to the issue (core.IssueTracker). */
  attachPullRequest?(teamId: string, issue: SourceRef, pr: PullRequestRef, factoryId?: string): Promise<void>;
}
