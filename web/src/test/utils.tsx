import type { ReactElement } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { vi } from "vitest";
import { ROUTER_FUTURE } from "../App";
import { AuthProvider } from "../auth/AuthContext";
import { FactoryLayout } from "../components/FactoryLayout";
import { ToastProvider } from "../components/Toast";

export type Call = { method: string; path: string; search: string; body: unknown; headers: Headers; credentials?: RequestCredentials };
type Responder = (call: Call) => { status?: number; body?: unknown } | undefined;

export const ME = {
  user: { id: "usr_1", email: "ada@example.test", name: "Ada", role: "editor" },
  team: { id: "team_1", name: "Acme" },
  role: "editor",
  login_providers: [],
};

/**
 * Installs a fetch stub. Routes map "METHOD /path" (path relative to the
 * origin, without query) to a JSON body or a responder function.
 */
export function mockFetch(routes: Record<string, Responder | Record<string, unknown> | unknown[]>) {
  const calls: Call[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const req = input instanceof Request ? input : new Request(String(input), init);
    const url = new URL(req.url);
    let body: unknown = undefined;
    const text = await req.clone().text();
    if (text) {
      try {
        body = JSON.parse(text);
      } catch {
        body = text;
      }
    }
    const call: Call = { method: req.method, path: url.pathname, search: url.search, body, headers: req.headers, credentials: init?.credentials ?? req.credentials };
    calls.push(call);
    const route = routes[`${req.method} ${url.pathname}`];
    let status = 200;
    let payload: unknown = route;
    if (typeof route === "function") {
      const r = (route as Responder)(call);
      status = r?.status ?? 200;
      payload = r?.body;
    } else if (route === undefined) {
      status = 404;
      payload = { code: "resource_not_found", detail: `no mock for ${req.method} ${url.pathname}` };
    }
    if (status === 204) return new Response(null, { status });
    return new Response(JSON.stringify(payload ?? {}), {
      status,
      headers: { "Content-Type": status >= 400 ? "application/problem+json" : "application/json" },
    });
  });
  vi.stubGlobal("fetch", fn);
  return { fn, calls };
}

/** Minimal EventSource double: tests push named events with emit(). */
export class MockEventSource {
  static instances: MockEventSource[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readonly url: string;
  readonly withCredentials: boolean;
  readyState = MockEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  private listeners = new Map<string, Set<(ev: Event) => void>>();
  constructor(url: string, init?: { withCredentials?: boolean }) {
    this.url = url;
    this.withCredentials = !!init?.withCredentials;
    MockEventSource.instances.push(this);
  }
  addEventListener(name: string, fn: (ev: Event) => void) {
    if (!this.listeners.has(name)) this.listeners.set(name, new Set());
    this.listeners.get(name)!.add(fn);
  }
  removeEventListener(name: string, fn: (ev: Event) => void) {
    this.listeners.get(name)?.delete(fn);
  }
  open() {
    this.readyState = MockEventSource.OPEN;
    this.onopen?.(new Event("open"));
  }
  emit(name: string, data: unknown, lastEventId = "") {
    const ev = new MessageEvent(name, { data: typeof data === "string" ? data : JSON.stringify(data), lastEventId });
    this.listeners.get(name)?.forEach((fn) => fn(ev));
  }
  close() {
    this.readyState = MockEventSource.CLOSED;
  }
  static latest(match: string): MockEventSource | undefined {
    return [...MockEventSource.instances].reverse().find((e) => e.url.includes(match) && e.readyState !== MockEventSource.CLOSED);
  }
}

export function installEventSource() {
  MockEventSource.instances = [];
  vi.stubGlobal("EventSource", MockEventSource);
}

export function renderAt(ui: ReactElement, { route, path }: { route: string; path: string }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <AuthProvider>
          <MemoryRouter initialEntries={[route]} future={ROUTER_FUTURE}>
            <Routes>
              <Route path={path} element={ui} />
            </Routes>
          </MemoryRouter>
        </AuthProvider>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

/** Renders a page inside FactoryLayout: route "/f/fac_1/<sub>", path "<sub pattern>". */
export function renderFactoryPage(ui: ReactElement, { route, path }: { route: string; path: string }) {
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
