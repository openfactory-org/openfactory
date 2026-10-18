import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { InboxPage } from "../pages/Inbox";
import { ME, installEventSource, mockFetch, renderAt } from "./utils";

const now = new Date().toISOString();
const ITEMS = [
  { id: "in_1", factory_id: "fac_1", work_item_id: "wi_1", run_id: "run_1", kind: "approval", title: "Approve the spec", body: "Spec for login fix", options: ["Approve", "Request changes"], recipients: ["usr_1"], is_read: false, resolved: false, created_at: now },
  { id: "in_2", factory_id: "fac_1", kind: "question", title: "Which database?", body: "Postgres or SQLite?", options: [], recipients: ["usr_1"], is_read: true, resolved: false, created_at: now },
  { id: "in_3", factory_id: "fac_1", kind: "failure", title: "Run failed", recipients: [], is_read: true, resolved: false, created_at: now },
];

describe("inbox actions", () => {
  beforeEach(() => installEventSource());
  afterEach(() => vi.unstubAllGlobals());

  it("approves, answers and resolves items", async () => {
    const resolved: Record<string, string> = {};
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [{ uid: "fac_1", id: "fac_1", name: "Payments", alias: "pay", source: "managed" }], page_info: { has_next_page: false } },
      "GET /api/v1/inbox": () => ({ body: { items: ITEMS.filter((i) => !resolved[i.id]), page_info: { has_next_page: false } } }),
      "POST /api/v1/inbox/in_1/resolve": (c) => {
        resolved.in_1 = (c.body as { resolution: string }).resolution;
        return { body: { ...ITEMS[0], resolved: true, resolution: resolved.in_1 } };
      },
      "POST /api/v1/inbox/in_2/resolve": (c) => {
        resolved.in_2 = (c.body as { resolution: string }).resolution;
        return { body: { ...ITEMS[1], resolved: true, resolution: resolved.in_2 } };
      },
      "POST /api/v1/inbox/in_3/resolve": (c) => {
        resolved.in_3 = (c.body as { resolution: string }).resolution;
        return { body: { ...ITEMS[2], resolved: true } };
      },
    });
    const user = userEvent.setup();
    renderAt(<InboxPage />, { route: "/inbox", path: "/inbox" });

    const approval = (await screen.findByText("Approve the spec")).closest("li")!;
    expect(within(approval).getByRole("link", { name: /view run/i })).toHaveAttribute("href", "/runs/run_1");
    expect(within(approval).getByRole("link", { name: /view work item/i })).toHaveAttribute("href", "/f/fac_1/work/wi_1");
    await user.click(within(approval).getByRole("button", { name: "Request changes" }));
    await waitFor(() => expect(resolved.in_1).toBe("Request changes"));
    await waitFor(() => expect(screen.queryByText("Approve the spec")).not.toBeInTheDocument());

    const question = screen.getByText("Which database?").closest("li")!;
    const answerBtn = within(question).getByRole("button", { name: /^answer$/i });
    expect(answerBtn).toBeDisabled();
    await user.type(within(question).getByLabelText("Answer"), "Postgres");
    await user.click(answerBtn);
    await waitFor(() => expect(resolved.in_2).toBe("Postgres"));

    const failure = screen.getByText("Run failed").closest("li")!;
    await user.click(within(failure).getByRole("button", { name: /mark resolved/i }));
    await waitFor(() => expect(resolved.in_3).toBe("resolved"));

    const posts = calls.filter((c) => c.method === "POST");
    expect(posts).toHaveLength(3);
    posts.forEach((p) => expect(p.headers.get("X-OF-CSRF")).toBe("1"));
    // The default view is the personal queue: the API default, without scope.
    expect(calls.find((c) => c.path === "/api/v1/inbox")!.search).not.toContain("scope=");
    expect(screen.queryByLabelText("Scope")).toBeInTheDocument();
    expect(await screen.findByText(/all caught up/i)).toBeInTheDocument();
  });

  it("applies a proposed definition change with one click on its review item", async () => {
    let resolution = "";
    const item = { id: "in_r", factory_id: "fac_1", run_id: "run_si", kind: "review", title: "Review definition change: run tests", body: "diff", options: ["approve", "reject"], recipients: ["usr_1"], is_read: true, resolved: false, created_at: now };
    mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/inbox": () => ({ body: { items: resolution ? [] : [item], page_info: { has_next_page: false } } }),
      "POST /api/v1/inbox/in_r/resolve": (c) => {
        resolution = (c.body as { resolution: string }).resolution;
        return { body: { ...item, resolved: true, resolution } };
      },
    });
    const user = userEvent.setup();
    renderAt(<InboxPage />, { route: "/inbox", path: "/inbox" });
    const card = (await screen.findByText("Review definition change: run tests")).closest("li")!;
    expect(within(card).queryByRole("button", { name: /mark resolved/i })).not.toBeInTheDocument();
    await user.click(within(card).getByRole("button", { name: "Approve and apply" }));
    await waitFor(() => expect(resolution).toBe("approve"));
  });

  it("filters by kind through the query string", async () => {
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/inbox": { items: [], page_info: { has_next_page: false } },
    });
    const user = userEvent.setup();
    renderAt(<InboxPage />, { route: "/inbox", path: "/inbox" });
    await screen.findByText(/all caught up/i);
    await user.selectOptions(screen.getByLabelText("Kind"), "question");
    await waitFor(() => expect(calls.some((c) => c.path === "/api/v1/inbox" && c.search.includes("kind=question"))).toBe(true));
  });

  it("marks items read, answers with the recommended option and undoes a resolve", async () => {
    const reads: boolean[] = [];
    let state: Record<string, unknown> = {
      id: "in_9", factory_id: "fac_1", run_id: "run_9", kind: "question", title: "Which branch?", body: "Pick one",
      questions: [{ question_id: "q1", text: "Which branch?", type: "single_select", options: ["main", "dev"], recommended_option: "dev" }],
      options: ["main", "dev"], recipients: ["usr_1"], is_read: false, resolved: false, created_at: now,
    };
    const { calls } = mockFetch({
      "GET /api/v1/me": ME,
      "GET /api/v1/factories": { items: [], page_info: { has_next_page: false } },
      "GET /api/v1/inbox": () => ({ body: { items: [state], page_info: { has_next_page: false } } }),
      "POST /api/v1/inbox/in_9/read": (c) => {
        reads.push((c.body as { read: boolean }).read);
        state = { ...state, is_read: (c.body as { read: boolean }).read };
        return { body: state };
      },
      "POST /api/v1/inbox/in_9/resolve": (c) => {
        state = { ...state, resolved: true, resolution: (c.body as { resolution: string }).resolution, resolved_by: "user:usr_1", resolved_via: "dashboard", resolved_at: new Date().toISOString() };
        return { body: state };
      },
      "POST /api/v1/inbox/in_9/unresolve": () => {
        state = { ...state, resolved: false, resolution: undefined, resolved_by: undefined, resolved_at: undefined };
        return { body: state };
      },
    });
    const user = userEvent.setup();
    renderAt(<InboxPage />, { route: "/inbox", path: "/inbox" });
    const card = (await screen.findByText("Which branch?")).closest("li")!;
    expect(within(card).getByText("Unread")).toBeInTheDocument();
    await user.click(within(card).getByRole("button", { name: "Which branch?" }));
    await waitFor(() => expect(reads).toEqual([true]));
    await user.click(await within(screen.getByText("Which branch?").closest("li")!).findByRole("button", { name: "dev (recommended)" }));
    // The resolved item and the toast both offer Undo.
    const undo = await screen.findAllByRole("button", { name: "Undo" });
    expect(undo.length).toBe(2);
    await user.click(undo[0]);
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.path === "/api/v1/inbox/in_9/unresolve")).toBe(true));
    expect(calls.find((c) => c.path === "/api/v1/inbox/in_9/resolve")!.body).toEqual({ resolution: "dev" });
  });
});
