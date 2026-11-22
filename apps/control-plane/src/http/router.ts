// A small method + path matcher over ROUTES. Literal segments win over
// parameters, so /secrets/rewrap is not read as /secrets/{name}.
import type { HttpMethod, RouteDef } from "./handler";

export const API_PREFIX = "/api/v1";

interface Compiled {
  def: RouteDef;
  segments: string[];
  /** Number of literal segments, for precedence. */
  literals: number;
}

export type MatchResult =
  | { kind: "match"; route: RouteDef; params: Record<string, string> }
  | { kind: "method_not_allowed"; allow: HttpMethod[] }
  | { kind: "not_found" };

export class Router {
  private readonly compiled: Compiled[];

  constructor(routes: readonly RouteDef[]) {
    this.compiled = routes
      .map((def) => {
        const segments = def.path.split("/").slice(1);
        return { def, segments, literals: segments.filter((s) => !s.startsWith("{")).length };
      })
      .sort((a, b) => b.literals - a.literals);
  }

  /** `path` is relative to /api/v1, for example "/runs/run_x". */
  match(method: string, path: string): MatchResult {
    const parts = path.split("/").slice(1);
    const allow = new Set<HttpMethod>();
    let best: { route: RouteDef; params: Record<string, string> } | undefined;
    for (const c of this.compiled) {
      const params = matchSegments(c.segments, parts);
      if (!params) continue;
      if (c.def.method === method || (method === "HEAD" && c.def.method === "GET")) {
        best ??= { route: c.def, params };
      } else {
        allow.add(c.def.method);
      }
    }
    if (best) return { kind: "match", ...best };
    if (allow.size > 0) return { kind: "method_not_allowed", allow: [...allow].sort() };
    return { kind: "not_found" };
  }
}

function matchSegments(pattern: string[], parts: string[]): Record<string, string> | undefined {
  if (pattern.length !== parts.length) return undefined;
  const params: Record<string, string> = {};
  for (let i = 0; i < pattern.length; i++) {
    const p = pattern[i]!;
    const v = parts[i]!;
    if (p.startsWith("{")) {
      if (v === "") return undefined;
      try {
        params[p.slice(1, -1)] = decodeURIComponent(v);
      } catch {
        return undefined;
      }
    } else if (p !== v) {
      return undefined;
    }
  }
  return params;
}
