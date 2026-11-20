import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkersPage } from "../pages/Team";
import { ME, mockFetch, renderAt } from "./utils";

const ADMIN = { ...ME, user: { ...ME.user, role: "admin" }, role: "admin" };
const DEAD = {
  id: "evt_1",
  topic: "event.inbound",
  partition_key: "event.inbound|team:team_1|github:acme/api#7",
  state: "dead",
  attempts: 8,
  last_error: "handler failed",
  created_at: new Date().toISOString(),
  summary: "issue_comment github:acme/api#7",
};

describe("dead-lettered events", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists dead events for an admin and delivers one again", async () => {
    let redriven = false;
    const { calls } = mockFetch({
      "GET /api/v1/me": ADMIN,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/workers": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/outbox/dead": () => ({ body: { items: redriven ? [] : [DEAD], page_info: { has_next_page: false } } }),
      "POST /api/v1/outbox/evt_1/redrive": () => {
        redriven = true;
        return { status: 204 };
      },
    });
    const user = userEvent.setup();
    renderAt(<WorkersPage />, { route: "/workers", path: "/workers" });
    expect(await screen.findByText("issue_comment github:acme/api#7")).toBeInTheDocument();
    expect(screen.getByText("handler failed")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Deliver again" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/api/v1/outbox/evt_1/redrive")).toBe(true));
    expect(await screen.findByText("No events failed every delivery attempt.")).toBeInTheDocument();
  });

  it("is hidden from a member who is not an admin", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/workers": { items: [], page_info: { has_next_page: false } },
    });
    renderAt(<WorkersPage />, { route: "/workers", path: "/workers" });
    expect(await screen.findByText("No remote workers have connected")).toBeInTheDocument();
    expect(screen.queryByText("Dead-lettered events")).not.toBeInTheDocument();
    expect(calls.some((c) => c.path === "/api/v1/outbox/dead")).toBe(false);
  });
});
