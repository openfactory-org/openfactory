import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { mergeSecrets, useUsableSecrets } from "../lib/secrets";
import { SecretsPage } from "../pages/Team";
import { ME, mockFetch, renderAt } from "./utils";

const now = new Date().toISOString();
const TEAM_TOKEN = { id: "sec_1", name: "CLAUDE_OAUTH_TOKEN", type: "anthropic_oauth_token", created_at: now, updated_at: now };
const TEAM_GH = { id: "sec_2", name: "GITHUB_TOKEN", type: "generic", created_at: now, updated_at: now };
const FAC_GH = { id: "sec_3", name: "GITHUB_TOKEN", type: "generic", factory_id: "fac_1", created_at: now, updated_at: now };
const page = (items: unknown[]) => ({ items, page_info: { has_next_page: false } });

/** Answers GET /secrets with team or factory secrets, depending on factory_id. */
function secretsRoute(c: { search: string }) {
  return { body: page(new URLSearchParams(c.search).get("factory_id") === "fac_1" ? [FAC_GH] : [TEAM_TOKEN, TEAM_GH]) };
}

describe("mergeSecrets", () => {
  it("merges by name and lets a factory secret win", () => {
    const got = mergeSecrets([TEAM_TOKEN, TEAM_GH, FAC_GH] as never, [FAC_GH] as never);
    expect(got.map((s) => [s.name, s.scope, s.id])).toEqual([
      ["CLAUDE_OAUTH_TOKEN", "team", "sec_1"],
      ["GITHUB_TOKEN", "factory", "sec_3"],
    ]);
  });
});

function Picker() {
  const s = useUsableSecrets("fac_1");
  return (
    <ul>
      {(s.data ?? []).map((x) => (
        <li key={x.name}>{`${x.name}:${x.scope}`}</li>
      ))}
    </ul>
  );
}

describe("useUsableSecrets", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("fetches team and factory secrets", async () => {
    const { calls } = mockFetch({ "GET /api/v1/me": ME, "GET /api/v1/secrets": secretsRoute });
    renderAt(<Picker />, { route: "/", path: "/" });
    expect(await screen.findByText("CLAUDE_OAUTH_TOKEN:team")).toBeInTheDocument();
    expect(await screen.findByText("GITHUB_TOKEN:factory")).toBeInTheDocument();
    const searches = calls.filter((c) => c.path === "/api/v1/secrets").map((c) => c.search);
    expect(searches).toEqual(expect.arrayContaining(["", "?factory_id=fac_1"]));
  });
});

describe("SecretsPage", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("replaces the value of a secret", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": page([]),
      "GET /api/v1/secrets": page([TEAM_TOKEN]),
      "PUT /api/v1/secrets/CLAUDE_OAUTH_TOKEN": TEAM_TOKEN,
    });
    const user = userEvent.setup();
    renderAt(<SecretsPage />, { route: "/secrets", path: "/secrets" });
    await user.click(await screen.findByRole("button", { name: "Replace value" }));
    const dialog = await screen.findByRole("dialog");
    const input = within(dialog).getByLabelText("New value", { exact: false });
    expect(input).toHaveAttribute("type", "password");
    expect(input).toHaveValue("");
    await user.type(input, "fake-new-value");
    await user.click(within(dialog).getByRole("button", { name: "Save value" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    const put = calls.find((c) => c.method === "PUT")!;
    expect(put.path).toBe("/api/v1/secrets/CLAUDE_OAUTH_TOKEN");
    expect(put.body).toEqual({ value: "fake-new-value" });
    expect(await screen.findByText("Secret CLAUDE_OAUTH_TOKEN updated.")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.queryByText("fake-new-value")).not.toBeInTheDocument();
  });
});
