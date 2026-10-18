import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ROUTER_FUTURE } from "../App";
import { AuthProvider } from "../auth/AuthContext";
import { FactoryLayout } from "../components/FactoryLayout";
import { ToastProvider } from "../components/Toast";
import { BenchmarkPage, ScorersPage } from "../pages/Measure";
import { ME, mockFetch } from "./utils";

const FACTORY = { uid: "fac_1", id: "fac_1", name: "Payments", alias: "pay", source: "managed" };
const now = new Date().toISOString();

function renderFactoryPage(ui: React.ReactElement, route: string, path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <AuthProvider>
          <MemoryRouter initialEntries={[route]} future={ROUTER_FUTURE}>
            <Routes>
              <Route path="/f/:fid" element={<FactoryLayout />}>
                <Route path={path} element={ui} />
              </Route>
            </Routes>
          </MemoryRouter>
        </AuthProvider>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("self-improvement proposals", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the diff, approves one proposal and rejects another with a comment", async () => {
    const decided: Record<string, unknown> = {};
    const proposals = [
      { id: "defv_a", state: "proposed", base_version_id: "defv_0", author_run_id: "run_si", message: "Tell implement to run tests", diff: "--- a/agents/implement/agent.md\n+++ b/agents/implement/agent.md\n+Run the tests.\n", base_is_active: true, created_at: now },
      { id: "defv_b", state: "proposed", base_version_id: "defv_0", author_run_id: "run_si", message: "Drop the review agent", diff: "-x\n", base_is_active: true, created_at: now },
    ];
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/factories/fac_1/scorers": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/scores": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/work-items": {
        items: [{ id: "wi_si", title: "Self-improvement: implement", source: { provider: "self_improvement" }, pull_requests: [{ url: "https://forge.example/acme/def/pull/7", repo: "acme/def", number: 7, state: "open" }] }],
        page_info: { has_next_page: false },
      },
      "GET /api/v1/factories/fac_1/definition/proposals": () => ({ body: { items: proposals.map((p) => ({ ...p, state: (decided[p.id] as string) ?? p.state })), page_info: { has_next_page: false } } }),
      "POST /api/v1/factories/fac_1/definition/proposals/defv_a/approve": () => {
        decided.defv_a = "approved";
        return { body: { ...proposals[0], state: "approved", applied_version_id: "defv_1" } };
      },
      "POST /api/v1/factories/fac_1/definition/proposals/defv_b/reject": (c) => {
        decided.defv_b = "rejected";
        decided.comment = (c.body as { comment: string }).comment;
        return { body: { ...proposals[1], state: "rejected" } };
      },
    });
    const user = userEvent.setup();
    renderFactoryPage(<ScorersPage />, "/f/fac_1/scorers", "scorers");

    const list = await screen.findByRole("list", { name: "Proposed definition changes" });
    expect(within(list).getByText(/\+Run the tests\./)).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "acme/def#7" })).toHaveAttribute("href", "https://forge.example/acme/def/pull/7");

    const first = within(list).getByText("Tell implement to run tests").closest("li")!;
    await user.click(within(first).getByRole("button", { name: "Approve and apply" }));
    await waitFor(() => expect(decided.defv_a).toBe("approved"));

    const second = within(list).getByText("Drop the review agent").closest("li")!;
    await user.type(within(second).getByLabelText(/Comment for/), "Keep it");
    await user.click(within(second).getByRole("button", { name: "Reject" }));
    await waitFor(() => expect(decided.comment).toBe("Keep it"));
  });

  it("cancels a running benchmark", async () => {
    let cancelled = false;
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1": FACTORY,
      "GET /api/v1/benchmark-runs/bench_1": () => ({ body: { id: "bench_1", factory_id: "fac_1", suite: "fix", state: cancelled ? "cancelled" : "scoring", created_at: now } }),
      "POST /api/v1/benchmark-runs/bench_1/cancel": () => {
        cancelled = true;
        return { body: { id: "bench_1", state: "cancelled" } };
      },
    });
    vi.stubGlobal("confirm", () => true);
    const user = userEvent.setup();
    renderFactoryPage(<BenchmarkPage />, "/f/fac_1/benchmarks/bench_1", "benchmarks/:id");
    await user.click(await screen.findByRole("button", { name: "Cancel benchmark" }));
    await waitFor(() => expect(cancelled).toBe(true));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Cancel benchmark" })).not.toBeInTheDocument());
  });
});
