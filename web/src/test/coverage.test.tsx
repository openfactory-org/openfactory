import { fireEvent, screen } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { FactoryLayout } from "../components/FactoryLayout";
import { coverageGaps, describeGaps, repoOwner, selectIntegration, type IntegrationLike } from "../lib/coverage";
import { DashboardPage } from "../pages/Dashboard";
import { AGENT_CATALOG, NewFactoryPage } from "../pages/Factories";
import { ME, mockFetch, renderAt } from "./utils";

const gh = (org: string, extra: Partial<IntegrationLike> = {}): IntegrationLike => ({ id: `int_${org}`, provider: "github", config: { org }, ...extra });

describe("repoOwner", () => {
  it.each([
    ["github", "acme/api", "acme"],
    ["gitlab", "acme/platform/api", "acme"],
    ["azure_devops", "org/project/repo", "org"],
    ["azure_devops", "project/repo", ""],
    ["git", "acme/api", ""],
    ["github", "api", ""],
  ])("%s %s -> %q", (provider, repo, want) => {
    expect(repoOwner(provider, repo)).toBe(want);
  });
});

describe("coverageGaps", () => {
  const cases: { name: string; repos: { codeForge?: "GITHUB" | "GITLAB" | "AZURE_DEVOPS"; owner: string; name: string }[]; ints: IntegrationLike[]; factory: string; want: string[] }[] = [
    { name: "owner of another org", repos: [{ owner: "acme", name: "api" }], ints: [gh("of-local-checks")], factory: "fac_1", want: ["acme/api:missing"] },
    { name: "team record of the owner, case ignored", repos: [{ owner: "Acme", name: "api" }], ints: [gh("acme")], factory: "fac_1", want: [] },
    { name: "record of this factory", repos: [{ owner: "acme", name: "api" }], ints: [gh("acme", { factory_id: "fac_1" })], factory: "fac_1", want: [] },
    { name: "record of another factory never counts", repos: [{ owner: "acme", name: "api" }], ints: [gh("acme", { factory_id: "fac_2" })], factory: "fac_1", want: ["acme/api:missing"] },
    { name: "new factory uses team records only", repos: [{ owner: "acme", name: "api" }], ints: [gh("acme", { factory_id: "fac_2" })], factory: "", want: ["acme/api:missing"] },
    { name: "disabled factory record wins over the team record", repos: [{ owner: "acme", name: "api" }], ints: [gh("acme"), gh("acme", { id: "int_f", factory_id: "fac_1", status: "disabled" })], factory: "fac_1", want: ["acme/api:inactive"] },
    { name: "auth_failed team record", repos: [{ owner: "acme", name: "api" }], ints: [gh("acme", { status: "auth_failed" })], factory: "fac_1", want: ["acme/api:inactive"] },
    { name: "two team records for one owner", repos: [{ owner: "acme", name: "api" }], ints: [gh("acme"), gh("acme", { id: "int_2" })], factory: "fac_1", want: ["acme/api:ambiguous"] },
    { name: "provider must match", repos: [{ codeForge: "GITLAB", owner: "acme", name: "api" }], ints: [gh("acme")], factory: "fac_1", want: ["acme/api:missing"] },
    { name: "gitlab subgroup uses the top-level group", repos: [{ codeForge: "GITLAB", owner: "acme/platform", name: "api" }], ints: [{ provider: "gitlab", config: { org: "acme" } }], factory: "fac_1", want: [] },
    { name: "azure devops project/name matches the single record", repos: [{ codeForge: "AZURE_DEVOPS", owner: "project", name: "repo" }], ints: [{ provider: "azure_devops", config: { org: "contoso" } }], factory: "", want: [] },
    { name: "azure devops org/project/name matches config.org", repos: [{ codeForge: "AZURE_DEVOPS", owner: "contoso/project", name: "repo" }], ints: [{ provider: "azure_devops", config: { org: "other" } }], factory: "", want: ["contoso/project/repo:missing"] },
  ];
  it.each(cases)("$name", ({ repos, ints, factory, want }) => {
    expect(coverageGaps(repos, ints, factory).map((g) => `${g.repo}:${g.reason}`)).toEqual(want);
  });

  it("selects the factory record before the team record", () => {
    const r = selectIntegration([gh("acme"), gh("acme", { id: "int_f", factory_id: "fac_1" })], "github", "fac_1", "acme");
    expect(r.record?.id).toBe("int_f");
    expect(r.reason).toBeUndefined();
  });

  it("describes the gaps by reason", () => {
    expect(
      describeGaps([
        { repo: "acme/api", reason: "missing" },
        { repo: "acme/web", reason: "missing" },
        { repo: "beta/x", reason: "inactive" },
      ]),
    ).toEqual(["No code host covers acme/api, acme/web.", "The code host record for beta/x is not active."]);
  });
});

const FACTORY = { id: "fac_1", uid: "fac_1", name: "Payments", alias: "pay", source: "managed", active_version_id: "v1" };

function dashboardRoutes(extra: Record<string, unknown> = {}) {
  return mockFetch({
    "GET /api/v1/me": ME,
    "GET /api/v1/factories/fac_1": FACTORY,
    "GET /api/v1/factories/fac_1/work-items": { items: [] },
    "GET /api/v1/factories/fac_1/metrics": { metrics: {} },
    "GET /api/v1/factories/fac_1/settings": { factory_id: "fac_1", editable: true, credential_strategy: "EXECUTOR", repositories: [{ codeForge: "GITHUB", owner: "acme", name: "api" }], machines: [], integration_types: [] },
    "GET /api/v1/integrations": { items: [{ id: "int_1", provider: "github", config: { org: "of-local-checks" } }] },
    "GET /api/v1/secrets": { items: [] },
    ...extra,
  });
}

function renderDashboard() {
  return renderAt(
    <Routes>
      <Route path="/f/:fid" element={<FactoryLayout />}>
        <Route index element={<DashboardPage />} />
      </Route>
    </Routes>,
    { route: "/f/fac_1", path: "*" },
  );
}

describe("get started checklist", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("keeps the code host step open and names the uncovered repositories", async () => {
    dashboardRoutes();
    renderDashboard();
    expect(await screen.findByText("No code host covers acme/api.")).toBeInTheDocument();
    const step = screen.getByText("Connect a code host").closest("li")!;
    expect(step).not.toHaveClass("done");
    expect(screen.getByRole("link", { name: "Connect" })).toHaveAttribute("href", "/integrations");
  });

  it("marks the step done when a record serves each repository", async () => {
    dashboardRoutes({ "GET /api/v1/integrations": { items: [{ id: "int_1", provider: "github", config: { org: "acme" } }] } });
    renderDashboard();
    await screen.findByText("Connect a code host");
    await vi.waitFor(() => expect(screen.getByText("Connect a code host").closest("li")).toHaveClass("done"));
    expect(screen.queryByText(/No code host covers/)).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Connect" })).not.toBeInTheDocument();
  });

  it("shows the credential hint only when no model credential secret exists", async () => {
    dashboardRoutes();
    renderDashboard();
    expect(await screen.findByText(/A run needs a model credential/)).toBeInTheDocument();
  });

  it("hides the credential hint when the team has a model credential secret", async () => {
    dashboardRoutes({ "GET /api/v1/secrets": { items: [{ id: "sec_1", name: "claude", type: "anthropic_oauth_token" }] } });
    renderDashboard();
    await screen.findByText("Connect a code host");
    await screen.findByText("No code host covers acme/api.");
    expect(screen.queryByText(/A run needs a model credential/)).not.toBeInTheDocument();
  });
});

describe("new factory form", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("labels the implement agent by its name", () => {
    expect(AGENT_CATALOG.find((a) => a.name === "implement")?.label).toBe("Implement");
  });

  it("warns about repositories that no code host covers, without blocking", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/integrations": { items: [{ id: "int_1", provider: "github", config: { org: "of-local-checks" } }] },
      "POST /api/v1/factories": { status: 201, body: { factory: { ...FACTORY } } },
    });
    renderAt(<NewFactoryPage />, { route: "/factories/new", path: "/factories/new" });
    await vi.waitFor(() => expect(calls.some((c) => c.path === "/api/v1/integrations")).toBe(true));
    fireEvent.change(screen.getByPlaceholderText("acme/api"), { target: { value: "of-local-checks/app\nacme/api" } });
    const warning = await screen.findByText(/No code host covers acme\/api\./);
    expect(warning).not.toHaveTextContent("of-local-checks/app");
    expect(screen.getByRole("link", { name: "Integrations" })).toHaveAttribute("href", "/integrations");

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Payments" } });
    fireEvent.click(screen.getByRole("button", { name: "Create factory" }));
    await vi.waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/api/v1/factories")).toBe(true));
  });

  it("shows no warning when every repository is covered", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/integrations": { items: [{ id: "int_1", provider: "github", config: { org: "acme" } }] },
    });
    renderAt(<NewFactoryPage />, { route: "/factories/new", path: "/factories/new" });
    await vi.waitFor(() => expect(calls.some((c) => c.path === "/api/v1/integrations")).toBe(true));
    fireEvent.change(screen.getByPlaceholderText("acme/api"), { target: { value: "beta/x" } });
    expect(await screen.findByText(/No code host covers beta\/x\./)).toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText("acme/api"), { target: { value: "acme/api" } });
    expect(screen.queryByText(/No code host covers/)).not.toBeInTheDocument();
  });
});
