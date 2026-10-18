import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App, makeQueryClient } from "../App";
import { safeReturnTo } from "../pages/LoginPage";
import { ME, installEventSource, mockFetch } from "./utils";

describe("login", () => {
  beforeEach(() => {
    installEventSource();
    window.history.pushState(null, "", "/f/fac_1/runs?x=1");
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("offers SSO with return_to and signs in with an API key", async () => {
    let signedIn = false;
    const { calls } = mockFetch({
      "GET /api/v1/me": () =>
        signedIn
          ? { body: ME }
          : { status: 401, body: { code: "authentication_required", login_providers: [{ name: "oidc", display_name: "Acme SSO", login_url: "/auth/oidc/login" }] } },
      "POST /auth/session": (c) => {
        signedIn = (c.body as { api_key?: string }).api_key === "of_abcdefgh_secret";
        return signedIn ? { status: 204 } : { status: 401, body: { detail: "invalid API key" } };
      },
      "GET /api/v1/factories": { items: [{ uid: "fac_1", id: "fac_1", name: "Payments", alias: "pay", source: "managed" }], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1": { uid: "fac_1", id: "fac_1", name: "Payments", alias: "pay", source: "managed" },
      "GET /api/v1/runs": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/inbox": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/version": { version: "1.2.3" },
    });
    const user = userEvent.setup();
    render(<App client={makeQueryClient()} />);

    const sso = await screen.findByRole("link", { name: /continue with acme sso/i });
    expect(sso).toHaveAttribute("href", `/auth/oidc/login?return_to=${encodeURIComponent("/f/fac_1/runs?x=1")}`);

    // A wrong key shows the server's error and stays on the login page.
    await user.type(screen.getByLabelText("API key"), "of_wrong");
    await user.click(screen.getByRole("button", { name: /sign in with api key/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/invalid API key/i);

    await user.clear(screen.getByLabelText("API key"));
    await user.type(screen.getByLabelText("API key"), "of_abcdefgh_secret");
    await user.click(screen.getByRole("button", { name: /sign in with api key/i }));

    // After sign-in the originally requested page renders inside the app shell.
    expect(await screen.findByRole("heading", { name: "Runs", level: 1 })).toBeInTheDocument();
    expect(window.location.pathname).toBe("/f/fac_1/runs");

    const session = calls.filter((c) => c.path === "/auth/session");
    expect(session).toHaveLength(2);
    expect(session[1].body).toEqual({ api_key: "of_abcdefgh_secret" });
    expect(session[1].headers.get("X-OF-CSRF")).toBe("1");
    expect(session[1].credentials).toBe("include");
    // Safe API calls never carry the CSRF header.
    const me = calls.find((c) => c.path === "/api/v1/me")!;
    expect(me.headers.get("X-OF-CSRF")).toBeNull();
  });

  it("signs out with POST /auth/logout", async () => {
    let signedIn = true;
    const { calls } = mockFetch({
      "GET /api/v1/me": () => (signedIn ? { body: ME } : { status: 401, body: { login_providers: [] } }),
      "POST /auth/logout": () => {
        signedIn = false;
        return { status: 204 };
      },
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/inbox": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/version": { version: "1.2.3" },
      "GET /api/v1/login-providers": { login_providers: [] },
    });
    window.history.pushState(null, "", "/factories");
    const user = userEvent.setup();
    render(<App client={makeQueryClient()} />);
    // Sign out is in the team menu of the sidebar.
    await user.click(await screen.findByRole("button", { name: /acme menu/i }));
    await user.click(await screen.findByRole("menuitem", { name: /sign out/i }));
    await waitFor(() => expect(screen.getByRole("button", { name: /sign in with api key/i })).toBeInTheDocument());
    const logout = calls.find((c) => c.path === "/auth/logout")!;
    expect(logout.method).toBe("POST");
    expect(logout.headers.get("X-OF-CSRF")).toBe("1");
  });
});

describe("safeReturnTo", () => {
  it.each([
    [null, "/factories"],
    ["/runs/run_1", "/runs/run_1"],
    ["//evil.example", "/factories"],
    ["https://evil.example", "/factories"],
    ["/\\evil", "/factories"],
    ["/login?x", "/factories"],
  ])("%s -> %s", (input, want) => {
    expect(safeReturnTo(input)).toBe(want);
  });
});
