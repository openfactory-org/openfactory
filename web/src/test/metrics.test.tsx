import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { MetricsView } from "../pages/Dashboard";
import { renderAt } from "./utils";

describe("metrics view", () => {
  it("shows pull request autonomy from facts, without an estimate", () => {
    renderAt(
      <MetricsView
        m={{
          autonomy: { merged_prs: 4, autonomous_merged_prs: 3, pr_rate: 0.75, rate: 0.5, completed_work_items: 2, autonomous_work_items: 1, merged_prs_without_facts: 2, facts_since: "2026-11-01T10:00:00Z" },
          cycle_time: { count: 2, median_ms: 3600000, pull_requests: { count: 4, reviewed: 2, start_to_merge_ms: 7200000, start_to_pr_ms: 1800000, pr_to_first_review_ms: 600000, first_review_to_merge_ms: 900000 } },
          pull_requests: { opened: 4, merged: 4, by_size: [{ size: "s", lines: "10-49", count: 3, total_cost_usd: 6, median_cost_usd: 2 }, { size: "xl", lines: "1000+", count: 0 }] },
        }}
      />,
      { route: "/", path: "/" },
    );
    expect(screen.getByText("3 of 4 merged PRs had no push by a person")).toBeInTheDocument();
    expect(screen.getByText(/Pull request figures start when the forge was connected/)).toBeInTheDocument();
    expect(screen.getByText(/2 merged PRs from before are not counted/)).toBeInTheDocument();
    expect(screen.getByText("Cost by PR size")).toBeInTheDocument();
    expect(screen.getByText("10-49")).toBeInTheDocument();
    expect(screen.queryByText("1000+")).not.toBeInTheDocument();
    expect(screen.getByText("PR to first review")).toBeInTheDocument();
    expect(document.body.textContent?.toLowerCase()).not.toContain("estimate");
  });

  it("says that the figures start when the forge is connected", () => {
    renderAt(<MetricsView m={{ autonomy: { merged_prs: 0 } }} />, { route: "/", path: "/" });
    expect(screen.getByText("Pull request figures start when the forge is connected.")).toBeInTheDocument();
  });

  it("shows compute time and instance shapes next to the cost", () => {
    renderAt(
      <MetricsView
        m={{
          cost: {
            total: { cost_usd: 4, compute_seconds: 3600 },
            compute_by_shape: { "linux/x86_64 4 vCPU 8 GB": { sessions: 3, seconds: 3000 }, default: { sessions: 1, seconds: 600 } },
          },
        }}
      />,
      { route: "/", path: "/" },
    );
    expect(screen.getByText("Compute time")).toBeInTheDocument();
    expect(screen.getByText("Compute by instance shape")).toBeInTheDocument();
    expect(screen.getByText("linux/x86_64 4 vCPU 8 GB")).toBeInTheDocument();
    expect(screen.getByText("default")).toBeInTheDocument();
  });

  it("counts failed and errored runs from the lower-case states of the API", () => {
    renderAt(<MetricsView m={{ runs: { total: 10, by_state: { succeeded: 6, failed: 1, error: 3 } } }} />, { route: "/", path: "/" });
    const tile = screen.getByText("Failed runs").parentElement!;
    expect(tile).toHaveTextContent("4");
    expect(tile).toHaveTextContent("40% of runs");
  });
});
