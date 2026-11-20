import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, errorMessage } from "../api/client";
import { ErrorAlert } from "../components/ui";
import { AgentPage } from "../pages/Agents";
import { AutomationPage, AutomationsPage, fmtUTC, setFrontmatterEnabled } from "../pages/Automations";
import { defaultChangeMessage } from "../pages/Definition";
import { changedIdentity, gitRepoError, SettingsPage } from "../pages/Settings";
import { EnvironmentPage } from "../pages/Team";
import { filterHeadline, WebhookPage, WebhooksPage, webhookUsage } from "../pages/Webhooks";
import { ME, mockFetch, renderAt, renderFactoryPage } from "./utils";

const FACTORY = { uid: "fac_1", id: "fac_1", name: "Payments", alias: "pay", description: "", source: "managed", active_version_id: "defv_1" };
const EMPTY = { items: [], page_info: { has_next_page: false } };
const REFUSED = {
  status: 400,
  body: {
    code: "invalid_request",
    detail: "definition could not be applied",
    errors: [
      { file: "webhooks/ci.yaml", line: 3, message: "secret CI_WEBHOOK_SECRET does not exist" },
      { file: "factory.yaml", field: "name", message: "is required" },
      { file: "agents/a.md", line: 1, message: "bad frontmatter" },
      { file: "agents/b.md", message: "unknown machine" },
    ],
  },
};

describe("refused definition changes", () => {
  it("lists the first validation errors in the error text", () => {
    const err = new ApiError(400, REFUSED.body);
    expect(errorMessage(err)).toBe(
      "definition could not be applied: webhooks/ci.yaml:3: secret CI_WEBHOOK_SECRET does not exist; factory.yaml name: is required; agents/a.md:1: bad frontmatter (and 1 more)",
    );
    expect(errorMessage(new ApiError(404, { detail: "not found" }))).toBe("not found");
  });

  it("shows the validation errors as a list in ErrorAlert", () => {
    render(<ErrorAlert error={new ApiError(400, REFUSED.body)} />);
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("definition could not be applied");
    expect(within(alert).getAllByRole("listitem")).toHaveLength(4);
    expect(alert).toHaveTextContent("webhooks/ci.yaml:3: secret CI_WEBHOOK_SECRET does not exist");
  });
});

describe("webhooks", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("explains how to send the credential for each mode", () => {
    const url = "https://factory.example.test/webhooks/w/wh_1";
    const token = webhookUsage({ auth_mode: "token", ingress_url: url });
    expect(token.lines[0]).toContain("Authorization: Bearer <token>");
    expect(token.curl).toContain(`curl -X POST "${url}"`);
    expect(token.curl).toContain('-H "Authorization: Bearer $TOKEN"');

    const urlToken = webhookUsage({ auth_mode: "url_token", ingress_url: `${url}/{token}` });
    expect(urlToken.curl).toContain(`curl -X POST "${url}/$TOKEN"`);

    const github = webhookUsage({ auth_mode: "signature", signature_scheme: "github", ingress_url: url });
    expect(github.lines[0]).toContain("X-Hub-Signature-256: sha256=<signature>");
    expect(github.lines[1]).toContain("HMAC-SHA256");
    expect(github.curl).toContain('-H "X-Hub-Signature-256: sha256=$SIG"');

    const sw = webhookUsage({ auth_mode: "signature", signature_scheme: "standard_webhooks", ingress_url: url });
    expect(sw.curl).toContain('-H "Webhook-Signature: v1,$SIG"');
    expect(sw.curl).toContain("${SECRET#whsec_}");

    expect(webhookUsage({ auth_mode: "signature", signature_scheme: "vercel", ingress_url: url }).lines[1]).toContain("HMAC-SHA1");
  });

  it("says that a match does not trigger a disabled automation", () => {
    expect(filterHeadline({ would_trigger: true, result: "Would trigger" }, false)).toEqual({ text: "Would match, but the automation is disabled", ok: false });
    expect(filterHeadline({ would_trigger: true, result: "Would trigger" }, true)).toEqual({ text: "Would trigger", ok: true });
    expect(filterHeadline({ would_trigger: false, result: "Would not trigger" })).toEqual({ text: "Would not trigger", ok: false });
  });

  it("shows why a create failed inside the dialog", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/factories/fac_1/webhooks": EMPTY,
      "POST /api/v1/factories/fac_1/webhooks": () => REFUSED,
    });
    const user = userEvent.setup();
    renderFactoryPage(<WebhooksPage />, { route: "/f/fac_1/webhooks", path: "webhooks" });
    await user.click((await screen.findAllByRole("button", { name: /Add webhook/ }))[0]);
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("Name"), "ci");
    await user.click(within(dialog).getByRole("button", { name: "Create webhook" }));
    expect(await within(dialog).findByText("webhooks/ci.yaml:3: secret CI_WEBHOOK_SECRET does not exist")).toBeInTheDocument();
  });

  it("shows the curl example and one delivery label", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/webhooks/wh_1": { id: "wh_1", name: "ci", auth_mode: "token", secret_name: "CI_WEBHOOK_SECRET", enabled: true, ingress_url: "https://factory.example.test/webhooks/w/wh_1" },
      "GET /api/v1/webhooks/wh_1/deliveries": { items: [{ id: "dlv_1", webhook_id: "wh_1", status: "accepted", label: "Accepted", received_at: new Date().toISOString() }], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/webhooks": EMPTY,
    });
    renderFactoryPage(<WebhookPage />, { route: "/f/fac_1/webhooks/wh_1", path: "webhooks/:id" });
    const usage = await screen.findByRole("group", { name: "How to call this webhook" });
    expect(usage).toHaveTextContent("Authorization: Bearer <token>");
    expect(usage).toHaveTextContent('curl -X POST "https://factory.example.test/webhooks/w/wh_1"');
    const row = await screen.findByRole("button", { name: /dlv_1/ });
    expect(row.textContent).not.toMatch(/Accepted\s*Accepted/i);
  });
});

describe("automations", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("sets enabled in the frontmatter", () => {
    expect(setFrontmatterEnabled("---\nenabled: true\nagent: foreman\n---\n\nDo it.\n", false)).toBe("---\nenabled: false\nagent: foreman\n---\n\nDo it.\n");
    expect(setFrontmatterEnabled("---\nagent: foreman\n---\nDo it.", true)).toBe("---\nenabled: true\nagent: foreman\n---\nDo it.");
    expect(setFrontmatterEnabled("Do it.", false)).toBe("---\nenabled: false\n---\n\nDo it.");
  });

  it("formats times in UTC", () => {
    expect(fmtUTC("2026-11-23T09:00:00Z")).toBe("2026-11-23 09:00 UTC");
    expect(fmtUTC(null)).toBe("—");
  });

  it("lists the next and last run of a schedule", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/factories/fac_1/webhooks": EMPTY,
      "GET /api/v1/factories/fac_1/automations": { items: [{ id: "aut_1", name: "weekly", enabled: true, triggers: [{ provider: "schedule", schedule: { cron: "0 9 * * 1" } }] }], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/schedules": {
        items: [{ id: "weekly", automation_id: "aut_1", automation_name: "weekly", name: "", cron: "0 9 * * 1", concurrency: "skip", enabled: true, paused: false, last_run_at: null, next_run_at: "2026-11-23T09:00:00Z" }],
        page_info: { has_next_page: false },
      },
    });
    renderFactoryPage(<AutomationsPage />, { route: "/f/fac_1/automations", path: "automations" });
    expect(await screen.findByText("2026-11-23 09:00 UTC")).toBeInTheDocument();
    expect(screen.getByText("Never")).toBeInTheDocument();
  });
});

describe("automation page", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("disables the automation through enabled: and pauses a schedule", async () => {
    const content = "---\nenabled: true\nagent: foreman\ntriggers:\n  - provider: schedule\n    schedule:\n      cron: \"0 9 * * 1\"\n---\n\nDo it.\n";
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/factories/fac_1/webhooks": EMPTY,
      "GET /api/v1/factories/fac_1/automations/weekly": { content, automation: { id: "aut_1", name: "weekly", enabled: true } },
      "PUT /api/v1/factories/fac_1/automations/weekly": (c) => ({ body: { content: (c.body as { content: string }).content } }),
      "GET /api/v1/factories/fac_1/schedules": {
        items: [{ id: "weekly", automation_id: "aut_1", automation_name: "weekly", name: "", cron: "0 9 * * 1", concurrency: "skip", enabled: true, paused: false, last_run_at: "2026-11-16T09:00:00Z", next_run_at: "2026-11-23T09:00:00Z" }],
        page_info: { has_next_page: false },
      },
      "POST /api/v1/factories/fac_1/schedules/weekly/pause": { id: "weekly", paused: true },
    });
    const user = userEvent.setup();
    renderFactoryPage(<AutomationPage />, { route: "/f/fac_1/automations/weekly", path: "automations/:name" });
    expect(await screen.findByText("Next run: 2026-11-23 09:00 UTC")).toBeInTheDocument();
    expect(screen.getByText("Last run: 2026-11-16 09:00 UTC")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Pause" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path.endsWith("/schedules/weekly/pause"))).toBe(true));
    await user.click(screen.getByRole("button", { name: "Disable" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    expect((calls.find((c) => c.method === "PUT")!.body as { content: string }).content).toContain("enabled: false\nagent: foreman");
  });
});

describe("definition editor", () => {
  it("names the default change message after the change", () => {
    expect(defaultChangeMessage({ isNew: true, del: false, selected: "", path: "agents/x.md" })).toBe("Add agents/x.md");
    expect(defaultChangeMessage({ isNew: false, del: true, selected: "agents/x.md", path: "agents/x.md" })).toBe("Delete agents/x.md");
    expect(defaultChangeMessage({ isNew: false, del: false, selected: "agents/x.md", path: "agents/x.md" })).toBe("Update agents/x.md");
    expect(defaultChangeMessage({ isNew: false, del: false, selected: "a.md", path: "b.md" })).toBe("Rename a.md to b.md");
  });
});

describe("settings", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("checks the git repository like the server", () => {
    for (const ok of ["acme/factory-def", "github.com/acme/factory-def", "https://gitlab.example.test/group/sub/defs.git"]) expect(gitRepoError(ok)).toBeNull();
    for (const bad of ["factory-def", "github.com/factory-def", "", "acme/"]) expect(gitRepoError(bad)).not.toBeNull();
  });

  it("sends only the changed identity fields", () => {
    const saved = { name: "Payments", alias: "pay", description: "", credential_strategy: "EXECUTOR" };
    expect(changedIdentity(saved, saved)).toEqual({});
    expect(changedIdentity({ ...saved, name: "Billing " }, saved)).toEqual({ name: "Billing" });
  });

  it("does not call the API when nothing changed, and confirms a save", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/factories/fac_1/settings": { repositories: [], machines: [], credential_strategy: "" },
      "PATCH /api/v1/factories/fac_1": (c) => ({ body: { ...FACTORY, ...(c.body as object) } }),
    });
    const user = userEvent.setup();
    renderFactoryPage(<SettingsPage />, { route: "/f/fac_1/settings", path: "settings" });
    const save = await screen.findByRole("button", { name: "Save" });
    await user.click(save);
    expect(await screen.findByText("Nothing changed, so nothing was saved.")).toBeInTheDocument();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);

    await user.type(screen.getByLabelText("Description"), "Card payments");
    await user.click(save);
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH")).toBe(true));
    expect(calls.find((c) => c.method === "PATCH")!.body).toEqual({ description: "Card payments" });
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
  });

  it("names the source radios and rejects a repository without an owner", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/factories/fac_1/settings": { repositories: [], machines: [], credential_strategy: "" },
    });
    const user = userEvent.setup();
    renderFactoryPage(<SettingsPage />, { route: "/f/fac_1/settings", path: "settings" });
    await user.click(await screen.findByRole("radio", { name: "A git repository (changes through pull requests)" }));
    const repo = screen.getByLabelText("Repository");
    expect(repo).toHaveAttribute("placeholder", "acme/factory-def");
    await user.type(repo, "factory-def");
    await user.click(screen.getByRole("button", { name: "Update source" }));
    expect(await screen.findByText(/must name owner\/repository/)).toBeInTheDocument();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);
  });
});

describe("agent form", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows a deleted environment and names its controls", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/factories/fac_1/agents/triage/spec": {
        name: "triage", path: "agents/triage.md", has_file: true, editable: true, description: "", harness_type: "", model: "", reasoning_level: "",
        auth_secret: "", machine: "", worker_host: "", environment_id: "env_gone", secrets: [], mcp_servers: "", idle_timeout_minutes: "", prompt: "", machines: [],
      },
      "GET /api/v1/factories/fac_1/agents/triage": { name: "triage", type: "TRIAGE" },
      "GET /api/v1/secrets": { items: [{ id: "sec_1", name: "API_KEY", type: "GENERIC" }], page_info: { has_next_page: false } },
      "GET /api/v1/workers": EMPTY,
      "GET /api/v1/environments": { items: [{ id: "env_1", name: "default" }], page_info: { has_next_page: false } },
    });
    renderFactoryPage(<AgentPage />, { route: "/f/fac_1/agents/triage", path: "agents/:name" });
    const env = (await screen.findByRole("combobox", { name: "Environment" })) as HTMLSelectElement;
    await waitFor(() => expect(env.selectedOptions[0]?.textContent).toBe("Missing environment (env_gone)"));
    expect(screen.getByRole("combobox", { name: "Harness" })).toBeInTheDocument();
    expect(await screen.findByRole("checkbox", { name: "API_KEY" })).toBeInTheDocument();
    expect(screen.getByLabelText("MCP servers")).toHaveAttribute("placeholder", "docs-search:\n  managedId: docs-search");
  });
});

describe("environment delete", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the server's refusal", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": EMPTY,
      "GET /api/v1/environments/env_1": { id: "env_1", name: "default" },
      "DELETE /api/v1/environments/env_1": () => ({ status: 409, body: { code: "conflict", detail: "environment env_1 is used by agent triage of factory Payments" } }),
    });
    // Without a ConfirmHost, confirm() falls back to window.confirm.
    const ask = vi.fn(() => true);
    vi.stubGlobal("confirm", ask);
    const user = userEvent.setup();
    renderAt(<EnvironmentPage />, { route: "/environments/env_1", path: "/environments/:id" });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    expect(ask).toHaveBeenCalledWith(expect.stringContaining("You cannot delete it while an agent uses it."));
    expect(await screen.findByText("environment env_1 is used by agent triage of factory Payments")).toBeInTheDocument();
  });
});
