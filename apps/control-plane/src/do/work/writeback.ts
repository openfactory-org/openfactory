// Outbound notices of a work item: replies to where the work item started
// (notice.responder) and the tracker write-back (notice.tracker). Both share
// one ordered lane and one sequence per work item, and leave through the
// outbox, so they commit with the change that caused them.
import {
  isTrackerIssue,
  ISSUE_STATE_DONE,
  ISSUE_STATE_IN_REVIEW,
  OUTBOUND_ATTACH_PULL_REQUEST,
  OUTBOUND_ISSUE_STATE,
  REF_ISSUE,
  REF_SOURCE,
  workItemRefs,
  type PullRequestRef,
  type SourceRef,
} from "@openfactory/core";
import type { WorkItem } from "../../engine/types";
import type { Fx } from "./fx";

/** The next notice sequence number of a work item; it orders the messages of the notice lane. */
function nextNoticeSeq(fx: Fx, wiId: string): number {
  const seq = Number(fx.repo.getMeta(`notice_seq:${wiId}`) ?? "0") + 1;
  fx.repo.setMeta(`notice_seq:${wiId}`, String(seq));
  return seq;
}

/** e.notify: a notice to where the work item started, ordered per work item (outbox lane, not a Queue). */
export function notify(
  fx: Fx,
  wi: WorkItem | undefined,
  kind: string,
  text: string,
  inboxId?: string,
  options?: string[],
): void {
  if (!wi) return;
  const seq = nextNoticeSeq(fx, wi.id);
  fx.enqueue({
    dest: "notice:",
    kind: "notice.responder",
    lane: `notice:${wi.id}`,
    msgId: `notice:${wi.id}:${seq}`,
    payload: {
      message: {
        work_item_id: wi.id,
        source: { ...wi.source },
        text,
        kind,
        factory_id: wi.factory_id,
        ...(inboxId ? { inbox_id: inboxId } : {}),
        ...(options?.length ? { options } : {}),
      },
    },
  });
}

/**
 * e.notifyTop: notify without the review thread. The message is a top-level
 * reply on the source, for example the completion notice.
 */
export function notifyTop(fx: Fx, wi: WorkItem | undefined, kind: string, text: string): void {
  if (!wi) return;
  const source: WorkItem["source"] = { ...wi.source };
  delete source["thread_ref"];
  notify(fx, { ...wi, source }, kind, text);
}

// ---------- tracker write-back (engine/references.go) ----------

/** trackerIssues: the linked issues of a work item, and its source when it is a tracker issue. */
export function trackerIssues(wi: WorkItem): SourceRef[] {
  const out: SourceRef[] = [];
  for (const r of workItemRefs(wi)) {
    if (r.kind === REF_ISSUE || (r.kind === REF_SOURCE && isTrackerIssue(r.source))) out.push(r.source);
  }
  return out;
}

/**
 * trackerOp: one write-back to a tracker. It goes through the outbox with the
 * other outbound messages (notice.tracker), so it commits with the change
 * that caused it.
 */
export function trackerOp(
  fx: Fx,
  wi: WorkItem,
  issue: SourceRef,
  kind: string,
  state: string,
  pr?: PullRequestRef,
): void {
  const seq = nextNoticeSeq(fx, wi.id);
  fx.enqueue({
    dest: "notice:",
    kind: "notice.tracker",
    lane: `notice:${wi.id}`,
    msgId: `notice:${wi.id}:${seq}`,
    payload: {
      message: {
        work_item_id: wi.id,
        factory_id: wi.factory_id,
        source: { ...issue },
        text: "",
        kind,
        ...(state ? { issue_state: state } : {}),
        ...(kind === OUTBOUND_ATTACH_PULL_REQUEST && pr ? { pull_request: pr } : {}),
      },
    },
  });
}

/** writeBackPullRequest: attaches a newly linked pull request to the tracker issues and moves them to in review. */
export function writeBackPullRequest(fx: Fx, wi: WorkItem, pr: PullRequestRef): void {
  for (const issue of trackerIssues(wi)) {
    trackerOp(fx, wi, issue, OUTBOUND_ATTACH_PULL_REQUEST, "", pr);
    if (pr.state !== "merged" && pr.state !== "closed")
      trackerOp(fx, wi, issue, OUTBOUND_ISSUE_STATE, ISSUE_STATE_IN_REVIEW);
  }
}

/** writeBackComplete: moves the tracker issues of a completed work item to done. */
export function writeBackComplete(fx: Fx, wi: WorkItem): void {
  for (const issue of trackerIssues(wi)) trackerOp(fx, wi, issue, OUTBOUND_ISSUE_STATE, ISSUE_STATE_DONE);
}
