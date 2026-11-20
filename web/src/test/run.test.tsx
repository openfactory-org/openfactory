import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RunPage, isTerminal, mergeMessages } from "../pages/Runs";
import { ME, MockEventSource, installEventSource, mockFetch, renderAt } from "./utils";

const RUN = {
  run_id: "run_1",
  id: "run_1",
  state: "RUNNING",
  phase: "running",
  title: "Fix login",
  name: "Fix login",
  agent_name: "implement",
  factory_id: "fac_1",
  prompt: "Fix the login bug",
  created_at: new Date().toISOString(),
  usage: { cost_usd: 0.42, input_tokens: 1200, output_tokens: 300, duration_ms: 65000 },
  config: { harness: { type: "claude", model: "claude-opus-5-5" } },
};

describe("run detail live transcript", () => {
  beforeEach(() => installEventSource());
  afterEach(() => vi.unstubAllGlobals());

  it("streams messages and state, then stops at done", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/runs/run_1": RUN,
      "GET /api/v1/runs": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/scorers": { items: [{ id: "scr_1", slug: "quality", name: "Quality" }], page_info: { has_next_page: false } },
      "POST /api/v1/runs/run_1/followups": { run_id: "run_1" },
      "POST /api/v1/runs/run_1/score": { id: "sc_1", label: "good", passed: true },
    });
    const user = userEvent.setup();
    renderAt(<RunPage />, { route: "/runs/run_1", path: "/runs/:id" });

    expect(await screen.findByRole("heading", { name: /fix login/i, level: 1 })).toBeInTheDocument();
    const es = await waitFor(() => {
      const s = MockEventSource.latest("/runs/run_1/events");
      expect(s).toBeDefined();
      return s!;
    });
    expect(es.url).toBe(`${window.location.origin}/api/v1/runs/run_1/events`);
    expect(es.withCredentials).toBe(true);

    act(() => {
      es.open();
      es.emit("run", RUN);
      es.emit("message", { run_id: "run_1", seq: 1, role: "user", content: "Fix the login bug", created_at: RUN.created_at }, "1");
      es.emit("message", { run_id: "run_1", seq: 2, role: "assistant", content: "Looking at auth.go now", created_at: RUN.created_at }, "2");
      // Duplicate delivery (replay after reconnect) must not duplicate the transcript.
      es.emit("message", { run_id: "run_1", seq: 2, role: "assistant", content: "Looking at auth.go now", created_at: RUN.created_at }, "2");
    });
    const transcript = await screen.findByRole("list", { name: /transcript/i });
    expect(within(transcript).getByText("Looking at auth.go now")).toBeInTheDocument();
    expect(within(transcript).getAllByRole("listitem").filter((li) => li.dataset.seq)).toHaveLength(2);
    expect(screen.getByText(/● Live/)).toBeInTheDocument();

    // Follow-up posts with the CSRF header.
    await user.type(screen.getByLabelText(/follow-up message/i), "Also add a test");
    await user.click(screen.getByRole("button", { name: /send follow-up/i }));
    await waitFor(() => expect(calls.some((c) => c.path === "/api/v1/runs/run_1/followups")).toBe(true));
    const fu = calls.find((c) => c.path === "/api/v1/runs/run_1/followups")!;
    expect(fu.body).toEqual({ message: "Also add a test" });
    expect(fu.headers.get("X-OF-CSRF")).toBe("1");

    // Scoring the run.
    await user.click(screen.getByRole("button", { name: /^score$/i }));
    await waitFor(() => expect(calls.find((c) => c.path === "/api/v1/runs/run_1/score")?.body).toEqual({ scorer_id: "scr_1" }));

    // Terminal state: badge updates, live indicator and follow-up form disappear, stream closes.
    act(() => {
      es.emit("message", { run_id: "run_1", seq: 3, role: "assistant", content: "Opened PR #12", created_at: RUN.created_at }, "3");
      es.emit("done", { ...RUN, state: "SUCCEEDED", phase: "succeeded", result: "Opened PR #12" });
    });
    await waitFor(() => expect(screen.queryByText(/● Live/)).not.toBeInTheDocument());
    expect(screen.getAllByText("Opened PR #12").length).toBeGreaterThan(0);
    expect(screen.getByText("Succeeded")).toBeInTheDocument();
    expect(screen.queryByLabelText(/follow-up message/i)).not.toBeInTheDocument();
    expect(es.readyState).toBe(MockEventSource.CLOSED);
  });
});

describe("mergeMessages", () => {
  it("orders by seq and dedupes", () => {
    const m = (seq: number, content = `m${seq}`) => ({ run_id: "r", seq, role: "assistant" as const, content });
    expect(mergeMessages([m(1), m(3)], [m(2), m(3, "new")]).map((x) => x.content)).toEqual(["m1", "m2", "new"]);
  });
});

describe("run states", () => {
  beforeEach(() => installEventSource());
  afterEach(() => vi.unstubAllGlobals());

  it("treats ERROR as finished and BLOCKED as not finished", () => {
    for (const s of ["SUCCEEDED", "FAILED", "ERROR", "CANCELLED"]) expect(isTerminal(s)).toBe(true);
    for (const s of ["QUEUED", "PENDING", "CLAIMED", "RUNNING", "BLOCKED"]) expect(isTerminal(s)).toBe(false);
  });

  it("shows the blocked reason and keeps the follow-up form", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/runs/run_1": { ...RUN, state: "BLOCKED", blocked_reason: "human_request" },
      "GET /api/v1/runs": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/scorers": { items: [], page_info: { has_next_page: false } },
    });
    renderAt(<RunPage />, { route: "/runs/run_1", path: "/runs/:id" });
    expect(await screen.findByText(/waiting for a person/i)).toBeInTheDocument();
    expect(screen.getAllByText(/waiting: human request/i).length).toBeGreaterThan(0);
    expect(screen.getByLabelText(/follow-up message/i)).toBeInTheDocument();
  });

  it("shows status_message for an ERROR run and hides the follow-up form", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/runs/run_1": {
        ...RUN,
        state: "ERROR",
        error: "old text",
        status_message: { message: "Sandbox lost", failure_code: "sandbox_lost", retryable: true },
      },
      "GET /api/v1/runs": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/scorers": { items: [], page_info: { has_next_page: false } },
    });
    renderAt(<RunPage />, { route: "/runs/run_1", path: "/runs/:id" });
    expect(await screen.findByText(/sandbox lost/i)).toBeInTheDocument();
    expect(screen.queryByText(/old text/)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/follow-up message/i)).not.toBeInTheDocument();
  });

  it.each([
    ["no usage", undefined],
    ["usage with a zero duration", { cost_usd: 0, input_tokens: 0, output_tokens: 0, duration_ms: 0 }],
  ])("says the agent did not start when a run ended before its first turn (%s)", async (_name, usage) => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/runs/run_1": { ...RUN, state: "ERROR", usage, started_at: undefined, finished_at: new Date().toISOString() },
      "GET /api/v1/runs": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/scorers": { items: [], page_info: { has_next_page: false } },
    });
    renderAt(<RunPage />, { route: "/runs/run_1", path: "/runs/:id" });
    const details = await screen.findByRole("complementary", { name: "Run details" });
    expect(within(details).getAllByText("Agent did not start")).toHaveLength(2);
  });
});
