// Test harness for plugin Workers. Import from "@openfactory/plugin-api/testing".
// No test framework is imported, so any runner can use it.

import { OfError } from "@openfactory/core";
import type { HostApi, IntegrationRecord, RepoRef } from "../types";

export interface FakeFactory {
  id: string;
  teamId: string;
  alias: string;
  name: string;
}

export interface FakeHostOptions {
  teams?: Array<{ id: string; name: string }>;
  integrations?: IntegrationRecord[];
  factories?: FakeFactory[];
  /** Repositories by factory ID. */
  repos?: Record<string, RepoRef[]>;
  /** Secrets by team ID, then name: { team_1: { GL_HOOK: "tok" } }. A "*" team matches every team. */
  secrets?: Record<string, Record<string, string>>;
}

export interface LogEntry {
  level: string;
  message: string;
  attrs?: Record<string, string>;
}

/**
 * An in-memory HostApi. getSecret allows only names that an integration record
 * references (SecretRef or a config value under a key that ends in "_ref"),
 * as the control plane does. The scope argument is a factory ID or "team:<id>".
 */
export class FakeHostApi implements HostApi {
  readonly logs: LogEntry[] = [];
  readonly secretReads: Array<{ scope: string; name: string }> = [];

  constructor(private readonly o: FakeHostOptions = {}) {}

  async log(
    level: "debug" | "info" | "warn" | "error",
    message: string,
    attrs?: Record<string, string>,
  ): Promise<void> {
    this.logs.push({ level, message, ...(attrs ? { attrs } : {}) });
  }

  private teamOfScope(scope: string): string {
    if (scope.startsWith("team:")) return scope.slice(5);
    return this.o.factories?.find((f) => f.id === scope)?.teamId ?? "";
  }

  private allowed(teamId: string, name: string): boolean {
    return (this.o.integrations ?? []).some(
      (r) =>
        r.teamId === teamId &&
        (r.secretRef === name || Object.entries(r.config).some(([k, v]) => k.endsWith("_ref") && v === name)),
    );
  }

  async getSecret(scope: string, name: string): Promise<string> {
    this.secretReads.push({ scope, name });
    const team = this.teamOfScope(scope);
    if (!this.allowed(team, name)) {
      throw new OfError("not_authorized", `plugin may not read secret "${name}" of team ${team}`);
    }
    const v = this.o.secrets?.[team]?.[name] ?? this.o.secrets?.["*"]?.[name];
    if (v === undefined) throw new OfError("resource_not_found", `secret ${name} not found`);
    return v;
  }

  async expandSecrets(scope: string, text: string): Promise<string> {
    const re = /\{\{\s*([A-Za-z_][A-Za-z0-9_./-]*)\s*\}\}/g;
    let out = "";
    let last = 0;
    for (const m of text.matchAll(re)) {
      out += text.slice(last, m.index) + (await this.getSecret(scope, m[1]!));
      last = m.index + m[0].length;
    }
    return out + text.slice(last);
  }

  async listTeams(): Promise<Array<{ id: string; name: string }>> {
    return this.o.teams ?? [];
  }

  async listIntegrations(provider?: string): Promise<IntegrationRecord[]> {
    const all = this.o.integrations ?? [];
    return provider ? all.filter((r) => r.provider.toLowerCase() === provider.toLowerCase()) : all;
  }

  async listFactories(): Promise<FakeFactory[]> {
    return this.o.factories ?? [];
  }

  async getFactory(factoryId: string): Promise<FakeFactory> {
    const f = this.o.factories?.find((x) => x.id === factoryId);
    if (!f) throw new OfError("resource_not_found", `factory ${factoryId} not found`);
    return f;
  }

  async getFactoryRepos(factoryId: string): Promise<RepoRef[]> {
    return this.o.repos?.[factoryId] ?? [];
  }
}

// ---------- fetch mock ----------

export interface RecordedCall {
  method: string;
  /** Path with the query, relative to the origin: "/api/v4/projects/x?y=1". */
  path: string;
  /** Path without the query, as sent (percent-encoding kept). */
  rawPath: string;
  query: URLSearchParams;
  headers: Headers;
  /** Parsed JSON body, or undefined. */
  body: Record<string, unknown> | unknown[] | undefined;
  text: string;
}

export type MockHandler = (call: RecordedCall) => Response | unknown | Promise<Response | unknown>;

export interface MockFetch {
  fetch: typeof fetch;
  calls: RecordedCall[];
}

/**
 * Builds a fetch mock. `routes` maps "METHOD /raw/path" (no query) to a handler.
 * A handler returns a Response, or a value that is sent as JSON (a string is sent as text).
 * An unmatched request answers 404 and is recorded.
 */
export function mockFetch(routes: Record<string, MockHandler>): MockFetch {
  const calls: RecordedCall[] = [];
  const impl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = new Request(input, init);
    const url = new URL(req.url);
    const text = await req.text();
    let body: RecordedCall["body"];
    if (text !== "") {
      try {
        body = JSON.parse(text) as RecordedCall["body"];
      } catch {
        body = undefined;
      }
    }
    const call: RecordedCall = {
      method: req.method,
      path: url.pathname + url.search,
      rawPath: url.pathname,
      query: url.searchParams,
      headers: req.headers,
      body,
      text,
    };
    calls.push(call);
    const handler = routes[`${req.method} ${url.pathname}`];
    if (!handler) return new Response("not found: " + req.method + " " + url.pathname, { status: 404 });
    const out = await handler(call);
    if (out instanceof Response) return out;
    if (typeof out === "string") return new Response(out, { status: 200 });
    if (out === undefined) return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
    return new Response(JSON.stringify(out), { status: 200, headers: { "content-type": "application/json" } });
  };
  return { fetch: impl as typeof fetch, calls };
}

/** Finds the first recorded call for "METHOD /raw/path". */
export function findCall(calls: RecordedCall[], key: string): RecordedCall | undefined {
  return calls.find((c) => `${c.method} ${c.rawPath}` === key);
}

/** A POST webhook Request with a JSON body. */
export function webhookRequest(url: string, payload: unknown, headers: Record<string, string> = {}): Request {
  return new Request(url, {
    method: "POST",
    headers: { "content-type": "application/json", ...headers },
    body: JSON.stringify(payload),
  });
}
