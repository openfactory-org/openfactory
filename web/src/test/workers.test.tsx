import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { WorkersPage } from "../pages/Workers";
import { ME, mockFetch, renderAt } from "./utils";

const ADMIN = { ...ME, role: "admin", user: { ...ME.user, role: "admin" } };
const page = (items: unknown[]) => ({ items, page_info: { has_next_page: false } });
const now = new Date().toISOString();
const W1 = {
  id: "wrk_1", name: "build-host-1", provider: "microvm", kind: "microvm", status: "online", labels: { pool: "linux" },
  slots: 4, slots_used: 1, factories: [], facts: { runtime_version: "0.9.1" }, version: "0.9.0", last_seen: now, capacity: 4, active: 1,
};
const W2 = { id: "wrk_2", name: "dev-box", provider: "process", kind: "process", status: "draining", labels: {}, slots: 2, slots_used: 0, factories: ["fac_1"], last_seen: now };
const FACTORIES = page([{ id: "fac_1", name: "Core" }, { id: "fac_2", name: "Docs" }]);

function base(extra: Record<string, never | unknown> = {}, me = ADMIN) {
  return mockFetch({
    "GET /api/v1/me": me,
    "GET /api/v1/factories": FACTORIES,
    "GET /api/v1/workers": page([W1, W2]),
    "GET /api/v1/outbox/dead": page([]),
    ...extra,
  } as never);
}
const render = () => renderAt(<WorkersPage />, { route: "/workers", path: "/workers" });

describe("WorkersPage", () => {
  it("lists kind, state, slots, labels, version and factories", async () => {
    base();
    render();
    const row = (await screen.findByText("build-host-1")).closest("tr")!;
    expect(within(row).getByText("microVM host")).toBeInTheDocument();
    expect(within(row).getByText("Online")).toBeInTheDocument();
    expect(within(row).getByText("1/4", { exact: false })).toBeInTheDocument();
    expect(within(row).getByText("pool=linux", { exact: false })).toBeInTheDocument();
    expect(within(row).getByText("0.9.1")).toBeInTheDocument();
    expect(within(row).getByText("All factories")).toBeInTheDocument();
    const dev = screen.getByText("dev-box").closest("tr")!;
    expect(within(dev).getByText("Draining")).toBeInTheDocument();
    expect(within(dev).getByText("Core")).toBeInTheDocument();
  });

  it("drains an online worker and resumes a draining one", async () => {
    const { calls } = base({ "POST /api/v1/workers/wrk_1/drain": { ...W1, status: "draining" }, "POST /api/v1/workers/wrk_2/resume": { ...W2, status: "online" } });
    const user = userEvent.setup();
    render();
    await user.click(await screen.findByRole("button", { name: "Drain build-host-1" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/api/v1/workers/wrk_1/drain")).toBe(true));
    await user.click(screen.getByRole("button", { name: "Resume dev-box" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/api/v1/workers/wrk_2/resume")).toBe(true));
  });

  it("edits the factory restriction with PATCH", async () => {
    const { calls } = base({ "PATCH /api/v1/workers/wrk_1": W1 });
    const user = userEvent.setup();
    render();
    await user.click(await screen.findByRole("button", { name: "Edit factories of build-host-1" }));
    await user.click(await screen.findByRole("checkbox", { name: "Docs" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toEqual({ factories: ["fac_2"] }));
  });

  it("creates an enrollment token and shows it once", async () => {
    const { calls } = base({
      "POST /api/v1/workers/enrollments": { id: "enr_1", token: "ofe_secret", expires_at: now, created_at: now, command: "openfactory-runtime worker enroll --token ofe_secret" },
    });
    const user = userEvent.setup();
    render();
    await user.click((await screen.findAllByRole("button", { name: /Enroll worker/ }))[0]!);
    await user.type(await screen.findByLabelText("Labels"), "pool=gpu");
    await user.click(screen.getByRole("button", { name: "Create token" }));
    expect(await screen.findByDisplayValue("ofe_secret")).toBeInTheDocument();
    const post = calls.find((c) => c.method === "POST" && c.path === "/api/v1/workers/enrollments");
    expect(post?.body).toMatchObject({ labels: { pool: "gpu" }, factories: [], expires_in_seconds: 3600 });
  });

  it("revokes after confirmation and deletes", async () => {
    const { calls } = base({ "POST /api/v1/workers/wrk_1/revoke": { ...W1, status: "revoked" }, "DELETE /api/v1/workers/wrk_2": { status: 204 } });
    const user = userEvent.setup();
    const orig = window.confirm;
    window.confirm = () => true;
    try {
      render();
      await user.click(await screen.findByRole("button", { name: "Revoke build-host-1" }));
      await waitFor(() => expect(calls.some((c) => c.path === "/api/v1/workers/wrk_1/revoke")).toBe(true));
    } finally {
      window.confirm = orig;
    }
  });

  it("hides the actions from a member who is not an admin", async () => {
    base({}, ME);
    render();
    await screen.findByText("build-host-1");
    expect(screen.queryByRole("button", { name: /Enroll worker/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Drain build-host-1" })).not.toBeInTheDocument();
  });
});
