import { act, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CLOCK_TICK_MS, TimeAgo } from "../components/ui";
import { FACTORIES_KEY } from "../lib/queries";
import { streamInvalidations } from "../lib/live";

describe("streamInvalidations", () => {
  it.each([
    { topic: "inbox", data: {}, want: [["inbox"]] },
    { topic: "workitem", data: { id: "wi_1", factory_id: "fac_1" }, want: [["workitems", "fac_1"], ["workitem", "wi_1"]] },
    { topic: "workitem", data: {}, want: [["workitems"]] },
    {
      topic: "run",
      data: { run_id: "run_1", work_item_id: "wi_1", factory_id: "fac_1", state: "ERROR" },
      want: [["runs"], ["run", "run_1"], ["workitem", "wi_1"], ["metrics", "fac_1"]],
    },
    { topic: "run", data: { run_id: "run_2" }, want: [["runs"], ["run", "run_2"]] },
    { topic: "definition", data: { id: "fac_1" }, want: [FACTORIES_KEY, ["factory", "fac_1"]] },
    { topic: "message", data: { run_id: "run_1" }, want: [] },
    { topic: "run", data: "not json", want: [["runs"]] },
  ])("$topic $data", ({ topic, data, want }) => {
    expect(streamInvalidations(topic, data)).toEqual(want);
  });
});

describe("TimeAgo", () => {
  afterEach(() => vi.useRealTimers());

  it("advances on the shared clock without other re-renders", () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-01-01T12:00:00Z"));
    render(
      <>
        <TimeAgo t="2026-01-01T12:00:00Z" />
        <TimeAgo t="2026-01-01T11:00:00Z" />
      </>,
    );
    expect(screen.getByText("just now")).toBeInTheDocument();
    expect(screen.getByText("1 hour ago")).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(4 * CLOCK_TICK_MS);
    });
    expect(screen.getByText("2 minutes ago")).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(1);
  });
});
