// Fakes for the integration tests: store, secrets, fetch router, linker.
import { OfError } from "@openfactory/core";
import type { Event, ExternalIdentity, Integration, IntegrationStatus, User } from "@openfactory/core";
import { Linker } from "./link";
import type { Fetch, IntegrationDeps, IntegrationStore, SecretReader, WorkSide } from "./types";

export class FakeStore implements IntegrationStore {
  integrations: Integration[] = [];
  users: User[] = [];
  identities = new Map<string, ExternalIdentity>();
  settings: Record<string, Record<string, Record<string, string>>> = {};
  kv = new Map<string, string>();
  async listIntegrations(teamId: string) {
    return this.integrations.filter((i) => teamId === "" || i.team_id === teamId);
  }
  async getIntegration(id: string) {
    return this.integrations.find((i) => i.id === id) ?? null;
  }
  async setIntegrationStatus(id: string, status: IntegrationStatus, reason: string) {
    const i = this.integrations.find((x) => x.id === id);
    if (!i) return false;
    i.status = status;
    i.status_reason = reason;
    return true;
  }
  async findExternalIdentity(p: string, id: string) {
    return this.identities.get(`${p}|${id}`) ?? null;
  }
  async getUser(id: string) {
    return this.users.find((u) => u.id === id) ?? null;
  }
  async getUserByEmail(email: string) {
    return this.users.find((u) => u.email.toLowerCase() === email.toLowerCase()) ?? null;
  }
  async linkExternalIdentity(x: ExternalIdentity) {
    const cur = this.identities.get(`${x.provider}|${x.external_id}`);
    if (cur && cur.user_id !== "" && cur.user_id !== x.user_id) return false;
    this.identities.set(`${x.provider}|${x.external_id}`, x);
    return true;
  }
  async upsertExternalIdentity(x: ExternalIdentity) {
    this.identities.set(`${x.provider}|${x.external_id}`, x);
  }
  async getFactoryIntegrations(f: string) {
    return this.settings[f] ?? {};
  }
  async kvGet(k: string) {
    return this.kv.get(k) ?? null;
  }
  async kvSet(k: string, v: string) {
    this.kv.set(k, v);
  }
}

export class FakeSecrets implements SecretReader {
  constructor(private readonly m: Record<string, string>) {}
  async get(_team: string, name: string): Promise<string> {
    const v = this.m[name];
    if (v === undefined) throw new OfError("resource_not_found", `secret ${name} not found`);
    return v;
  }
}

export interface Call {
  method: string;
  url: string;
  headers: Headers;
  body: string;
}

/** A fetch that answers from a list of [matcher, responder] and records every call. */
export class FakeFetch {
  calls: Call[] = [];
  private routes: { match: (c: Call) => boolean; reply: (c: Call) => Response | object }[] = [];
  on(match: string | ((c: Call) => boolean), reply: (c: Call) => Response | object): this {
    const m = typeof match === "string" ? (c: Call) => c.url.includes(match) || c.body.includes(match) : match;
    this.routes.push({ match: m, reply });
    return this;
  }
  fetch: Fetch = async (input, init) => {
    const req = new Request(input as string, init);
    const call: Call = { method: req.method, url: req.url, headers: req.headers, body: await req.text() };
    this.calls.push(call);
    // The newest route wins, so a test can override a default.
    for (const r of [...this.routes].reverse()) {
      if (!r.match(call)) continue;
      const out = r.reply(call);
      return out instanceof Response ? out : Response.json(out);
    }
    return new Response("no route: " + call.url, { status: 599 });
  };
  json(i: number): Record<string, unknown> {
    return JSON.parse(this.calls[i]!.body) as Record<string, unknown>;
  }
}

export const NOW = new Date("2026-01-02T03:04:05Z");

export interface Kit {
  store: FakeStore;
  published: Event[];
  deps: IntegrationDeps;
  ff: FakeFetch;
}

export function kit(secrets: Record<string, string>, extra: Partial<IntegrationDeps> & { work?: WorkSide } = {}): Kit {
  const store = new FakeStore();
  const published: Event[] = [];
  const ff = new FakeFetch();
  const linker = new Linker({
    store,
    key: "test-link-key",
    publicUrl: "https://of.example.com",
    currentUser: async () => null,
    now: () => NOW,
  });
  const deps: IntegrationDeps = {
    store,
    secrets: new FakeSecrets(secrets),
    publish: async (e) => {
      published.push(...e);
    },
    fetch: ff.fetch,
    linker,
    now: () => NOW,
    log: { debug() {}, info() {}, warn() {}, error() {} },
    ...extra,
  };
  return { store, published, deps, ff };
}

export function integration(over: Partial<Integration> & Pick<Integration, "id" | "provider">): Integration {
  return { team_id: "team_1", name: over.provider, config: {}, created_at: "2026-01-01T00:00:00Z", ...over };
}

export function user(over: Partial<User> = {}): User {
  return {
    id: "usr_1",
    team_id: "team_1",
    email: "a@example.com",
    name: "A",
    role: "editor",
    created_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

export function post(url: string, body: string, headers: Record<string, string> = {}): Request {
  return new Request(url, { method: "POST", body, headers });
}
