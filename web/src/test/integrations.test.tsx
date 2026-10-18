import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { IntegrationsPage } from "../pages/Team";
import { ME, mockFetch, renderAt } from "./utils";

const now = new Date().toISOString();

describe("Integrations", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the status and the factory of each record", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": { ...ME, user: { ...ME.user, role: "admin" }, role: "admin" },
      "GET /api/v1/factories": { items: [{ id: "fac_1", name: "payments", team_id: "team_1" }], page_info: { has_next_page: false } },
      "GET /api/v1/integrations": {
        items: [
          { id: "int_1", provider: "github", name: "team app", config: { oauth_client_id: "Iv1.x" }, created_at: now },
          { id: "int_2", provider: "github", name: "payments app", factory_id: "fac_1", status: "disabled", status_reason: "the app was removed", created_at: now },
          { id: "int_3", provider: "gitlab", name: "old token", status: "auth_failed", created_at: now },
        ],
        page_info: { has_next_page: false },
      },
      "PATCH /api/v1/integrations/int_2": (c) => ({ status: 200, body: { id: "int_2", provider: "github", ...(c.body as object) } }),
    });
    renderAt(<IntegrationsPage />, { route: "/integrations", path: "/integrations" });
    expect(await screen.findByText("team app")).toBeInTheDocument();
    expect(screen.getByText("Active")).toBeInTheDocument();
    expect(screen.getByText("Disabled")).toBeInTheDocument();
    expect(screen.getByText("Auth failed")).toBeInTheDocument();
    expect(screen.getByText("the app was removed", { exact: false })).toBeInTheDocument();
    expect(await screen.findByRole("cell", { name: "payments" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Link your github account" })).toHaveAttribute("href", "/integrations/link-start/github");
    expect(screen.queryByRole("link", { name: "Link your gitlab account" })).toBeNull();
    const user = userEvent.setup();
    await user.click((await screen.findAllByRole("button", { name: "Enable" }))[0]);
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH" && c.path === "/api/v1/integrations/int_2")).toBe(true));
    expect(calls.find((c) => c.method === "PATCH")!.body).toEqual({ status: "active" });
  });
});
