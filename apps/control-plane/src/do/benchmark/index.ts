// BenchmarkDO: C0 stub. Owner: WS13. Replace the body; keep the class name and export
// (wrangler.jsonc "exports" declares it). Migrations go in ./migrations/NNNN_<name>.sql.
import { DurableObject } from "cloudflare:workers";
import { notImplemented, problemResponse } from "@openfactory/core";
import type { Env } from "../../contracts/env";

export class BenchmarkDO extends DurableObject<Env> {
  /** Liveness probe for tests and the readiness check. */
  async ping(): Promise<string> {
    return "BenchmarkDO";
  }

  override async fetch(_request: Request): Promise<Response> {
    return problemResponse(notImplemented("BenchmarkDO"));
  }
}
