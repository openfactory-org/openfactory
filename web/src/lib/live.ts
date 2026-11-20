import type { QueryKey } from "@tanstack/react-query";
import { FACTORIES_KEY } from "./queries";

/** The topics of the team's live stream that the dashboard subscribes to. */
export const STREAM_TOPICS = ["inbox", "workitem", "run", "definition"] as const;

type StreamData = { id?: string; run_id?: string; factory_id?: string; work_item_id?: string };

/**
 * Returns the query keys to invalidate when the live stream reports a change.
 * A run event also refreshes the pages that embed the run: the work item
 * detail (its `runs`) and the factory metrics (runs by state).
 */
export function streamInvalidations(topic: string, data: unknown): QueryKey[] {
  const d = (data && typeof data === "object" ? data : {}) as StreamData;
  switch (topic) {
    case "inbox":
      return [["inbox"]];
    case "workitem":
      return [d.factory_id ? ["workitems", d.factory_id] : ["workitems"], ...(d.id ? [["workitem", d.id]] : [])];
    case "run":
      return [
        ["runs"],
        ...(d.run_id ? [["run", d.run_id]] : []),
        ...(d.work_item_id ? [["workitem", d.work_item_id]] : []),
        ...(d.factory_id ? [["metrics", d.factory_id]] : []),
      ];
    case "definition":
      return [FACTORIES_KEY, ...(d.id ? [["factory", d.id]] : [])];
    default:
      return [];
  }
}
