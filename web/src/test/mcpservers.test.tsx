import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { McpServerPage, McpServersPage } from "../pages/McpServers";
import { ME, mockFetch, renderAt } from "./utils";

const now = new Date().toISOString();
const NOTION = {
  id: "mcp_1",
  team_id: "team_1",
  name: "notion",
  url: "https://mcp.notion.test/mcp",
  connection: "proxy",
  oauth: { status: "not_connected" },
  created_at: now,
  updated_at: now,
};

describe("MCP servers", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists registered and built-in servers", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/mcp-servers": {
        items: [NOTION],
        builtin: [
          { managed_id: "builtin/linear", provider: "linear", configured: true },
          { managed_id: "builtin/slack", provider: "slack", configured: false },
        ],
        page_info: { has_next_page: false },
      },
    });
    renderAt(<McpServersPage />, { route: "/mcp-servers", path: "/mcp-servers" });
    expect(await screen.findByText("notion")).toBeInTheDocument();
    expect(screen.getByText("OAuth: not connected")).toBeInTheDocument();
    expect(screen.getByText("builtin/linear")).toBeInTheDocument();
    expect(screen.getByText("Configured")).toBeInTheDocument();
    expect(screen.getByText("Not configured")).toBeInTheDocument();
  });

  it("registers a proxied server with OAuth", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "POST /api/v1/mcp-servers": (c) => ({ status: 201, body: { ...NOTION, ...(c.body as object) } }),
      "GET /api/v1/mcp-servers/mcp_1": NOTION,
    });
    const user = userEvent.setup();
    renderAt(<McpServerPage />, { route: "/mcp-servers/new", path: "/mcp-servers/:id" });
    await user.type(await screen.findByLabelText("Name", { exact: false }), "notion");
    await user.type(screen.getByLabelText("URL", { exact: false }), "https://mcp.notion.test/mcp");
    await user.click(screen.getByLabelText("The server uses OAuth"));
    await user.type(screen.getByLabelText("Scopes", { exact: false }), "read write");
    await user.click(screen.getByRole("button", { name: "Register server" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/api/v1/mcp-servers")).toBe(true));
    const body = calls.find((c) => c.method === "POST" && c.path === "/api/v1/mcp-servers")!.body as Record<string, unknown>;
    expect(body).toMatchObject({ name: "notion", url: "https://mcp.notion.test/mcp", connection: "proxy", command: "", oauth: { scopes: ["read", "write"] } });
  });

  it("registers a url server with the proxy by default, and a legacy SSE server as direct", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "POST /api/v1/mcp-servers": (c) => ({ status: 201, body: { ...NOTION, oauth: undefined, id: "mcp_2", ...(c.body as object) } }),
      "GET /api/v1/mcp-servers/mcp_2": { ...NOTION, oauth: undefined, id: "mcp_2" },
    });
    const user = userEvent.setup();
    renderAt(<McpServerPage />, { route: "/mcp-servers/new", path: "/mcp-servers/:id" });
    await user.type(await screen.findByLabelText("Name", { exact: false }), "sentry");
    await user.type(screen.getByLabelText("URL", { exact: false }), "https://mcp.sentry.test/sse");
    expect(screen.getByLabelText(/Route through the control plane/)).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Register server" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    expect(calls.find((c) => c.method === "POST")!.body).toMatchObject({ name: "sentry", connection: "direct" });
  });

  it("checks the proxy box for a new url server", async () => {
    mockFetch({ "GET /api/v1/me": ME, "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } } });
    renderAt(<McpServerPage />, { route: "/mcp-servers/new", path: "/mcp-servers/:id" });
    await userEvent.setup().type(await screen.findByLabelText("URL", { exact: false }), "https://mcp.sentry.test/mcp");
    expect(screen.getByLabelText(/Route through the control plane/)).toBeChecked();
  });

  it("shows the connection state and starts OAuth", async () => {
    const assign = vi.fn();
    vi.stubGlobal("location", { ...window.location, assign });
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/mcp-servers/mcp_1": NOTION,
      "POST /api/v1/mcp-servers/mcp_1/oauth/connect": { authorization_url: "https://idp.test/authorize?state=x" },
    });
    const user = userEvent.setup();
    renderAt(<McpServerPage />, { route: "/mcp-servers/mcp_1?oauth=error&message=denied", path: "/mcp-servers/:id" });
    expect(await screen.findByText(/The connection failed: denied/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Connect" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("https://idp.test/authorize?state=x"));
  });
});
