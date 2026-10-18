import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SpendCard } from "../components/SpendCard";
import { ME, mockFetch, renderAt } from "./utils";

describe("spend card", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the spend and each limit with a meter", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1/spend": {
        factory_id: "fac_1",
        factory_daily_usd: 8.5,
        factory_monthly_usd: 40,
        limits: [
          { scope: "factory", period: "daily", spent_usd: 8.5, limit_usd: 10, resets_at: "2026-11-12T00:00:00Z" },
          { scope: "team", period: "monthly", spent_usd: 600, limit_usd: 500, resets_at: "2026-12-01T00:00:00Z" },
        ],
      },
    });
    renderAt(<SpendCard fid="fac_1" />, { route: "/", path: "/" });
    expect(await screen.findByText("Factory daily limit")).toBeInTheDocument();
    const meters = screen.getAllByRole("meter");
    expect(meters).toHaveLength(2);
    expect(meters[0]).toHaveAttribute("aria-valuetext", "85% used");
    expect(screen.getByText(/Reached\. New runs wait in the queue/)).toBeInTheDocument();
  });

  it("says when no limit is set", async () => {
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories/fac_1/spend": { factory_id: "fac_1", factory_daily_usd: 1, factory_monthly_usd: 2, limits: [] },
    });
    renderAt(<SpendCard fid="fac_1" />, { route: "/", path: "/" });
    expect(await screen.findByText(/No spend limit/)).toBeInTheDocument();
    expect(screen.queryByRole("meter")).not.toBeInTheDocument();
  });
});
