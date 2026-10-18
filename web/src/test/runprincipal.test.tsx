import { screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RunPage, principalLabel } from "../pages/Runs";
import { ME, installEventSource, mockFetch, renderAt } from "./utils";

const RUN = {
  run_id: "run_1",
  id: "run_1",
  state: "SUCCEEDED",
  phase: "succeeded",
  title: "Nightly checks",
  name: "Nightly checks",
  agent_name: "foreman",
  factory_id: "fac_1",
  prompt: "Run the nightly checks",
  created_at: new Date().toISOString(),
  usage: { cost_usd: 0, input_tokens: 0, output_tokens: 0, duration_ms: 0 },
  config: { harness: { type: "claude", model: "claude-opus-5-5" } },
  creator: { type: "service_account", id: "sa_ci", name: "ci" },
  run_as: { type: "service_account", id: "sa_ci", name: "ci" },
};

describe("run principals", () => {
  beforeEach(() => installEventSource());
  afterEach(() => vi.unstubAllGlobals());

  it("names each type of principal", () => {
    expect(principalLabel({ type: "user", id: "usr_1", name: "Dana" })).toBe("Dana");
    expect(principalLabel({ type: "service_account", id: "sa_1", name: "ci" })).toBe("ci (service account)");
    expect(principalLabel({ type: "factory", id: "fac_1" })).toBe("fac_1 (factory)");
  });

  it("shows the creator and the principal of a run", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/runs/run_1": RUN,
      "GET /api/v1/runs": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/factories/fac_1/scorers": { items: [], page_info: { has_next_page: false } },
    });
    renderAt(<RunPage />, { route: "/runs/run_1", path: "/runs/:id" });
    expect(await screen.findByText("Runs as")).toBeInTheDocument();
    expect(screen.getByText("ci (service account)")).toBeInTheDocument();
    expect(screen.getByText("Creator")).toBeInTheDocument();
  });
});
