// A small fetch mock for provider adapters. Adapters take a `fetch` function in
// their config, so tests pass `mock.fetch` and register routes:
//
//   const mock = new FetchMock("https://api.example.test");
//   mock.on("GET", "/sandbox/:id", ({ params }) => FetchMock.json({ id: params.id }));
//
// Unmatched requests answer 599 and are recorded, so a test can assert on them.

export interface MockRequest {
  method: string;
  url: URL;
  path: string;
  params: Record<string, string>;
  headers: Headers;
  body: string;
  json<T = unknown>(): T;
}

export type MockHandler = (req: MockRequest) => Response | Promise<Response>;

export interface RecordedCall {
  method: string;
  url: string;
  path: string;
  headers: Record<string, string>;
  body: string;
}

interface Route {
  method: string;
  match: (path: string) => Record<string, string> | undefined;
  handler: MockHandler;
  once: boolean;
}

function compile(pattern: string | RegExp): (path: string) => Record<string, string> | undefined {
  if (pattern instanceof RegExp) {
    return (p) => {
      const m = pattern.exec(p);
      return m ? { ...(m.groups ?? {}) } : undefined;
    };
  }
  const names: string[] = [];
  const re = new RegExp(
    "^" +
      pattern
        .split("/")
        .map((seg) => {
          if (seg.startsWith(":")) {
            names.push(seg.slice(1));
            return "([^/]+)";
          }
          return seg.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
        })
        .join("/") +
      "$",
  );
  return (p) => {
    const m = re.exec(p);
    if (!m) return undefined;
    const out: Record<string, string> = {};
    names.forEach((n, i) => (out[n] = decodeURIComponent(m[i + 1]!)));
    return out;
  };
}

export class FetchMock {
  readonly calls: RecordedCall[] = [];
  readonly unmatched: RecordedCall[] = [];
  private routes: Route[] = [];
  private readonly basePath: string;

  constructor(base = "") {
    this.basePath = base ? new URL(base).pathname.replace(/\/$/, "") : "";
  }

  /** Registers a route. Later routes win over earlier ones. */
  on(method: string, pattern: string | RegExp, handler: MockHandler): this {
    this.routes.unshift({ method: method.toUpperCase(), match: compile(pattern), handler, once: false });
    return this;
  }

  /** Registers a route that answers once, then falls through to the others. */
  once(method: string, pattern: string | RegExp, handler: MockHandler): this {
    this.routes.unshift({ method: method.toUpperCase(), match: compile(pattern), handler, once: true });
    return this;
  }

  reset(): void {
    this.routes = [];
    this.calls.length = 0;
    this.unmatched.length = 0;
  }

  /** Calls whose method and path match. */
  find(method: string, path: string | RegExp): RecordedCall[] {
    const m = compile(path);
    return this.calls.filter((c) => c.method === method.toUpperCase() && m(c.path) !== undefined);
  }

  readonly fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = new Request(input, init);
    const url = new URL(req.url);
    let path = url.pathname;
    if (this.basePath && path.startsWith(this.basePath)) path = path.slice(this.basePath.length) || "/";
    const body = req.body ? await req.text() : "";
    const headers: Record<string, string> = {};
    req.headers.forEach((v, k) => (headers[k] = v));
    const call: RecordedCall = { method: req.method, url: req.url, path, headers, body };
    this.calls.push(call);
    for (const r of this.routes) {
      if (r.method !== req.method) continue;
      const params = r.match(path);
      if (!params) continue;
      if (r.once) this.routes = this.routes.filter((x) => x !== r);
      return r.handler({
        method: req.method,
        url,
        path,
        params,
        headers: req.headers,
        body,
        json: <T>() => JSON.parse(body || "null") as T,
      });
    }
    this.unmatched.push(call);
    return new Response(`no mock for ${req.method} ${path}`, { status: 599 });
  };

  static json(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
    return new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json", ...headers },
    });
  }

  static empty(status = 204, headers: Record<string, string> = {}): Response {
    return new Response(null, { status, headers });
  }
}
