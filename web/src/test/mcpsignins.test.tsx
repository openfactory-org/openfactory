import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { McpSignIns } from "../components/McpSignIns";
import { isServerPath, safeReturnTo } from "../pages/LoginPage";
import { ME, mockFetch, renderAt } from "./utils";

const GRANT = {
  id: "mcps_1",
  user_id: "usr_1",
  team_id: "team_1",
  client_id: "ofc_1",
  client_name: "Coding agent",
  created_at: new Date().toISOString(),
  expires_at: new Date(Date.now() + 86400000).toISOString(),
};

describe("Factory MCP sign-ins", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists the sign-ins and ends one", async () => {
    let revoked = false;
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/mcp-sign-ins": () => ({ body: { items: revoked ? [] : [GRANT], page_info: { has_next_page: false } } }),
      "DELETE /api/v1/mcp-sign-ins/mcps_1": () => {
        revoked = true;
        return { status: 204 };
      },
    });
    const user = userEvent.setup();
    renderAt(<McpSignIns />, { route: "/", path: "/" });
    expect(await screen.findByText("Coding agent")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "End sign-in" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.path === "/api/v1/mcp-sign-ins/mcps_1")).toBe(true));
    expect(await screen.findByText("No active sign-ins.")).toBeInTheDocument();
  });

  it("returns to the server sign-in page after the dashboard login", () => {
    const back = "/oauth/authorize?client_id=ofc_1&state=x";
    expect(safeReturnTo(back)).toBe(back);
    expect(isServerPath(back)).toBe(true);
    expect(isServerPath("/factories")).toBe(false);
  });
});
