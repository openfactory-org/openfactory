import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App, makeQueryClient } from "../App";
import { humanizeError } from "../lib/format";
import { ME, installEventSource, mockFetch } from "./utils";

const FACTORY = { uid: "fac_1", id: "fac_1", name: "Payments", alias: "pay", source: "managed" };

function mockShell() {
  return mockFetch({
    "GET /api/v1/me": ME,
    "GET /api/v1/factories": { items: [FACTORY], page_info: { has_next_page: false } },
    "GET /api/v1/factories/fac_1": FACTORY,
    "GET /api/v1/inbox": { items: [], page_info: { has_next_page: false } },
    "GET /api/v1/runs": { items: [], page_info: { has_next_page: false } },
    "GET /api/v1/version": { version: "1.2.3" },
    "GET /api/v1/factories/fac_1/work-items": { items: [], page_info: { has_next_page: false } },
    "GET /api/v1/users": { items: [] },
    "POST /api/v1/factories/fac_1/runs": { run_id: "run_9", work_item_id: "wi_9" },
    "GET /api/v1/work-items/wi_9": { id: "wi_9", title: "Fix the flaky test", stage: "intake", timeline: [], runs: [], inbox: [] },
  });
}

describe("app shell", () => {
  beforeEach(() => {
    installEventSource();
    // cmdk measures its list and scrolls the selected item into view.
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        unobserve() {}
        disconnect() {}
      },
    );
    Element.prototype.scrollIntoView = () => {};
  });
  afterEach(() => vi.unstubAllGlobals());

  it("creates a work item from the keyboard: C, type, send", async () => {
    const { calls } = mockShell();
    window.history.pushState(null, "", "/runs");
    const user = userEvent.setup();
    render(<App client={makeQueryClient()} />);
    expect(await screen.findByRole("heading", { name: "Runs", level: 1 })).toBeInTheDocument();

    await user.keyboard("c");
    const request = await screen.findByLabelText("Request");
    await user.type(request, "Fix the flaky test");
    await user.click(screen.getByRole("button", { name: /send to foreman/i }));

    await waitFor(() => expect(window.location.pathname).toBe("/f/fac_1/work/wi_9"));
    expect(calls.find((c) => c.method === "POST" && c.path === "/api/v1/factories/fac_1/runs")!.body).toEqual({ prompt: "Fix the flaky test" });
    expect(await screen.findByRole("heading", { name: "Fix the flaky test", level: 1 })).toBeInTheDocument();
  });

  it("opens the command palette and goes to a page", async () => {
    mockShell();
    window.history.pushState(null, "", "/runs");
    const user = userEvent.setup();
    render(<App client={makeQueryClient()} />);
    await screen.findByRole("heading", { name: "Runs", level: 1 });

    await user.click(screen.getByRole("button", { name: "Search and commands" }));
    await user.type(await screen.findByPlaceholderText(/type a command/i), "inbox");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(window.location.pathname).toBe("/inbox"));
  });

  it("goes to the inbox with G then I, and ignores keys typed in a field", async () => {
    mockShell();
    window.history.pushState(null, "", "/runs");
    const user = userEvent.setup();
    render(<App client={makeQueryClient()} />);
    await screen.findByRole("heading", { name: "Runs", level: 1 });

    await user.type(screen.getByLabelText("Agent"), "gi");
    expect(window.location.pathname).toBe("/runs");

    await user.click(document.body);
    await user.keyboard("gi");
    await waitFor(() => expect(window.location.pathname).toBe("/inbox"));
  });
});

describe("humanizeError", () => {
  it("takes the message out of a wrapped provider error", () => {
    const raw = 'agent_failed: model call failed: anthropic: HTTP 503: {"code":"inference_unavailable","message":"model provider anthropic is not configured"}';
    expect(humanizeError(raw)).toEqual({ message: "Model provider anthropic is not configured", detail: raw });
  });

  it("keeps a plain message as it is", () => {
    expect(humanizeError("Sandbox lost")).toEqual({ message: "Sandbox lost", detail: "" });
    expect(humanizeError("")).toEqual({ message: "", detail: "" });
  });
});
