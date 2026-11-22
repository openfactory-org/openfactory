// The generic work item states of the engine mapped to the states of an Azure
// DevOps work item type. Each type has its own list of states, and each state
// has a category: Proposed, InProgress, Resolved, Completed or Removed.
// Port of plugins/azuredevops/internal/azuredevops/tracker.go.

export interface TypeState {
  name: string;
  category: string;
}

export type IssueState = "in_progress" | "in_review" | "done";

/**
 * The state to set for a generic state, given the states of the work item type
 * and the current state. Returns "" when the work item stays as it is.
 */
export function pickState(state: string, states: TypeState[], current: string): string {
  const category = (name: string) => states.find((s) => s.name.toLowerCase() === name.toLowerCase())?.category ?? "";
  const first = (match: (s: TypeState) => boolean) => states.find(match)?.name ?? "";
  const cur = category(current);
  const finished = cur === "Completed" || cur === "Removed";
  let target = "";
  switch (state) {
    case "done":
      if (finished) return "";
      target = first((s) => s.category === "Completed");
      break;
    case "in_review":
      if (finished) return "";
      target = first(
        (s) => (s.category === "InProgress" || s.category === "Resolved") && s.name.toLowerCase().includes("review"),
      );
      if (target === "" && cur === "Proposed") {
        // No review state: the work item is at least in progress.
        target = first((s) => s.category === "InProgress");
      }
      break;
    case "in_progress":
      if (cur !== "Proposed") return ""; // it is already in progress, or further
      target = first((s) => s.category === "InProgress");
      break;
    default:
  }
  if (target === "" || target.toLowerCase() === current.toLowerCase()) return "";
  return target;
}
