import { render, renderHook, screen, waitFor, within } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App, makeQueryClient } from "../App";
import { streamInvalidations } from "../lib/live";
import { INBOX_COUNT_KEY } from "../lib/queries";
import { APP_TITLE, documentTitle, TitleScopeContext, useDocumentTitle } from "../lib/title";
import { InboxPage } from "../pages/Inbox";
import { hiddenLines, TranscriptMessage } from "../pages/Runs";
import { ME, installEventSource, mockFetch, renderAt } from "./utils";

const FACTORY = { uid: "fac_1", id: "fac_1", name: "Payments", alias: "pay", source: "managed", active_version_id: "ver_2" };
const EMPTY = { items: [], page_info: { has_next_page: false } };

function mockApp(extra: Record<string, unknown> = {}) {
  return mockFetch({
    "GET /api/v1/me": ME,
    "GET /api/v1/factories": { items: [FACTORY], page_info: { has_next_page: false } },
    "GET /api/v1/factories/fac_1": FACTORY,
    "GET /api/v1/inbox": EMPTY,
    "GET /api/v1/runs": EMPTY,
    "GET /api/v1/version": { version: "1.2.3" },
    "GET /api/v1/users": { items: [] },
    ...(extra as Record<string, Record<string, unknown>>),
  });
}

function renderApp(path: string) {
  window.history.pushState(null, "", path);
  return render(<App client={makeQueryClient()} />);
}

describe("documentTitle", () => {
  it.each([
    { parts: ["Agents", "Payments"], want: "Agents · Payments · Open Factory" },
    { parts: ["Inbox", undefined], want: "Inbox · Open Factory" },
    { parts: ["Payments", "Payments"], want: "Payments · Open Factory" },
    { parts: [undefined, "Payments"], want: "Payments · Open Factory" },
    { parts: ["  ", null], want: "Open Factory" },
  ])("$parts", ({ parts, want }) => {
    expect(documentTitle(parts)).toBe(want);
  });
});

describe("useDocumentTitle", () => {
  it("sets the tab title with the scope and restores it on unmount", () => {
    const wrapper = ({ children }: { children: ReactNode }) => <TitleScopeContext.Provider value="Payments">{children}</TitleScopeContext.Provider>;
    const { rerender, unmount } = renderHook(({ page }) => useDocumentTitle(page), { wrapper, initialProps: { page: "Runs" } });
    expect(document.title).toBe("Runs · Payments · Open Factory");
    rerender({ page: "Agents" });
    expect(document.title).toBe("Agents · Payments · Open Factory");
    unmount();
    expect(document.title).toBe(APP_TITLE);
  });
});

describe("not-found pages", () => {
  beforeEach(() => installEventSource());
  afterEach(() => vi.unstubAllGlobals());

  it.each([
    { path: "/f/fac_gone", heading: "Factory not found", link: "Go to factories", href: "/factories" },
    { path: "/f/fac_1/agents/nope", heading: "Agent not found", link: "Go to agents", href: "/f/fac_1/agents" },
    { path: "/runs/run_gone", heading: "Run not found", link: "Go to runs", href: "/runs" },
  ])("$path", async ({ path, heading, link, href }) => {
    // mockFetch answers 404 for every route it does not know.
    mockApp();
    renderApp(path);
    expect(await screen.findByRole("heading", { name: heading, level: 1 })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: link })).toHaveAttribute("href", href);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("keeps other errors as an alert", { timeout: 10_000 }, async () => {
    mockApp({ "GET /api/v1/factories/fac_1": () => ({ status: 500, body: { detail: "database is down" } }) });
    renderApp("/f/fac_1");
    // The client retries a 5xx answer twice before it shows the error.
    expect(await screen.findByRole("alert", {}, { timeout: 8000 })).toHaveTextContent("database is down");
    expect(screen.queryByRole("heading", { name: "Factory not found" })).not.toBeInTheDocument();
  });
});

describe("factory pages", () => {
  beforeEach(() => installEventSource());
  afterEach(() => vi.unstubAllGlobals());

  it("names the author of a version like the list and shows the factory in the breadcrumb", async () => {
    mockApp({
      "GET /api/v1/factories/fac_1/definition/versions": { items: [{ id: "ver_2", author: "user:usr_1", message: "Add a reviewer", valid: true, created_at: new Date().toISOString() }] },
      "GET /api/v1/factories/fac_1/definition/versions/ver_2": { id: "ver_2", author: "user:usr_1", message: "Add a reviewer", valid: true, files: {}, created_at: new Date().toISOString() },
    });
    renderApp("/f/fac_1/definition/versions/ver_2");
    expect(await screen.findByText(/Add a reviewer · You ·/)).toBeInTheDocument();
    expect(screen.queryByText(/user:usr_1/)).not.toBeInTheDocument();
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumbs).getByRole("link", { name: "Payments" })).toHaveAttribute("href", "/f/fac_1");
    expect(within(crumbs).getByRole("link", { name: "Version history" })).toBeInTheDocument();
    await waitFor(() => expect(document.title).toBe("Version ver_2 · Payments · Open Factory"));
  });

  it("labels the stage filter like the board", async () => {
    mockApp({ "GET /api/v1/factories/fac_1/work-items": EMPTY });
    renderApp("/f/fac_1/work");
    const stage = await screen.findByLabelText("Stage");
    const labels = within(stage).getAllByRole("option").map((o) => o.textContent);
    expect(labels).toContain("Awaiting approval");
    expect(labels).toContain("Handoff");
    expect(labels).not.toContain("awaiting approval");
  });
});

describe("shell", () => {
  beforeEach(() => installEventSource());
  afterEach(() => vi.unstubAllGlobals());

  it("names the inbox link with its badge count", async () => {
    mockApp({ "GET /api/v1/inbox": { items: [{ id: "in_1" }, { id: "in_2" }], page_info: { has_next_page: false } } });
    renderApp("/runs");
    const sidebar = await screen.findByRole("navigation", { name: "Main" });
    expect(await within(sidebar).findByRole("link", { name: "Inbox, 2 waiting" })).toHaveAttribute("href", "/inbox");
    expect(within(sidebar).getByRole("link", { name: "Runs" })).toBeInTheDocument();
    expect(within(sidebar).getByRole("button", { name: "Search and commands" })).toBeInTheDocument();
  });
});

describe("inbox badge", () => {
  it("is refreshed by an inbox stream event", () => {
    const qc = new QueryClient();
    qc.setQueryData(INBOX_COUNT_KEY, 3);
    for (const queryKey of streamInvalidations("inbox", { id: "in_1" })) void qc.invalidateQueries({ queryKey });
    expect(qc.getQueryState(INBOX_COUNT_KEY)?.isInvalidated).toBe(true);
  });
});

describe("transcript tool rows", () => {
  it.each([
    { s: ".openfactory", want: 0 },
    { s: ".openfactory\nsrc\n\nweb\n", want: 2 },
    { s: "\n\nonly\n", want: 0 },
    { s: "", want: 0 },
  ])("hiddenLines($s)", ({ s, want }) => {
    expect(hiddenLines(s)).toBe(want);
  });

  it("shows how many lines the summary hides", () => {
    render(
      <ol>
        <TranscriptMessage m={{ seq: 1, role: "tool_result", content: ".openfactory\nsrc\nweb", created_at: new Date().toISOString() } as never} />
      </ol>,
    );
    expect(screen.getByText(".openfactory")).toBeInTheDocument();
    expect(screen.getByText("… 2 more lines")).toBeInTheDocument();
  });
});

describe("inbox question", () => {
  beforeEach(() => installEventSource());
  afterEach(() => vi.unstubAllGlobals());

  it("does not repeat a body that equals the title and labels the answer chips", async () => {
    const q = "Should I say hello or goodbye?";
    mockApp({
      "GET /api/v1/inbox": {
        items: [{ id: "in_1", factory_id: "fac_1", kind: "question", title: q, body: q, options: ["hello", "goodbye"], recipients: ["usr_1"], is_read: true, resolved: false, created_at: new Date().toISOString() }],
        page_info: { has_next_page: false },
      },
    });
    renderAt(<InboxPage />, { route: "/inbox", path: "/inbox" });
    const item = (await screen.findByRole("button", { name: q })).closest("li")!;
    expect(within(item).getAllByText(q)).toHaveLength(1);
    expect(within(item).getByRole("button", { name: "Answer: goodbye" })).toHaveAttribute("title", 'Send "goodbye" as the answer');
  });
});
