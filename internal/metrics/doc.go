// Package metrics computes the factory Dashboard: activity, pull requests,
// autonomy, cycle time, human wait time, run counts, cost and Scorer results
// over a date range.
//
// Compute loads data from a core.Store (Collect) and derives the dashboard
// with the pure function Build, so every metric can be tested without a store.
//
// # Window
//
// All ranges are half-open: [since, until), in UTC. Daily buckets are UTC
// calendar days.
//
// # Definitions
//
// Work items by stage: work items created in the window, grouped by their
// current stage. Created / Completed / Cancelled count items created, completed
// (CompletedAt in window, stage complete) and cancelled (CompletedAt in window,
// stage cancelled) in the window.
//
// PRs opened: distinct pull requests (deduplicated by forge, repo and number,
// or URL) linked to the factory's work items, counted once in the period they
// were first observed. First observation is the earliest "pr_linked" work-item
// event for that PR; without such an event, the work item's creation time.
//
// PRs merged: of the PRs opened in the window, those whose state is now
// "merged". (So, as in the product, the merged count is a property of the
// opening cohort, not of merges that happened in the window.)
//
// Cost per PR: a work item's cost is the sum of the usage of all its runs
// (the foreman run and every descendant), split evenly across the work item's
// PRs. The dashboard reports the median over PRs opened in the window, plus the
// most expensive PRs. This is a comparison metric, not a billing figure.
//
// Autonomy: the share of work items completed in the window that needed no
// human intervention beyond required approvals. A work item needed
// intervention when any of the following is true:
//
//   - an inbox item of kind question, handoff or failure was raised for it
//     (ask_human, an explicit handoff to a person, or a failure escalation);
//   - its timeline contains a "question" or "answer" event;
//   - a person sent a further message ("message_in" event) after the work
//     started, i.e. after its first "run_started" event or after the initial
//     request message;
//   - it was taken over locally (LocalOwner set).
//
// Approval requests and decisions (approval inbox items, "approval_requested"
// and "approval_decided" events), review items, comments, PR reviews and the
// merge itself are required human checkpoints and do not count as
// intervention.
//
// PR autonomy: of the factory's PRs that merged in the window and have pull
// request facts (core.PullRequestFacts, recorded by RecordEvent from forge
// events), the share that no person opened or pushed to before the merge.
// Merged PRs without facts (events from before the forge was connected) are
// counted in MergedPRsWithoutFacts and not in the rate.
//
// PR cycle time: for the PRs that merged in the window and have facts, the
// medians of work item start → merge, start → PR opened, PR opened → first
// review and first review → merge. Each part is an independent median.
//
// Cost by PR size: the cost of the PRs opened in the window, grouped by
// changed lines (xs < 10, s < 50, m < 250, l < 1000, xl, unknown).
//
// Cycle time: for work items completed (stage complete) in the window,
// CompletedAt − CreatedAt; the median, 90th percentile (nearest rank) and mean
// are reported. Time to PR is the median of first PR observation − CreatedAt
// over PRs opened in the window. Stage medians are the median time completed
// items spent in each stage, from "stage_changed" events; like the product's
// stage medians they are independent and don't sum to the headline.
//
// Human wait time: inbox items of kind approval, question, handoff or failure
// created in the window. Wait is ResolvedAt − CreatedAt, or until − CreatedAt
// for items still pending. Median and P90 cover resolved items; Total includes
// pending waits up to until.
//
// Runs: runs created in the window by agent, state, agent×state, model and
// source (the TriggeredBy prefix: automation, api, mcp, run, benchmark,
// scorer, self_improvement...). Totals include evaluation, benchmark and
// Self-improvement runs.
//
// Cost and tokens: usage of runs created in the window, by agent and by model.
//
// Scorer pass rates: scores created in the window per scorer, with label counts.
package metrics
