// RouterDO (key team:<t>|router): source keys and references -> work item,
// with the work-item status (C6). resolveOrReserve is atomic and returns the
// same token while a route is reserved or active (plan rule 4).
import { hashToken, newRoutedId } from "@openfactory/core";
import type { OutboxPayloads } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { BaseDO, type Migration } from "../../lib/base";
import { first } from "../../lib/sql/query";

/** Completed work items reopen within this window (references.go). */
export const REOPEN_WINDOW_MS = 7 * 24 * 3_600_000;
const RESERVE_TTL_MS = 10 * 60_000;

const m0001: Migration = {
  version: 1,
  name: "init",
  sql: `
CREATE TABLE routes (factory_id TEXT NOT NULL, source_key TEXT NOT NULL, work_item_id TEXT NOT NULL, work_token TEXT NOT NULL,
  kind TEXT NOT NULL, status TEXT NOT NULL, completed_ms INTEGER, wi_version INTEGER NOT NULL DEFAULT 0, updated_ms INTEGER NOT NULL,
  PRIMARY KEY (factory_id, source_key, work_item_id));
CREATE UNIQUE INDEX routes_one_active_source ON routes (factory_id, source_key)
  WHERE kind = 'source' AND status IN ('reserved', 'active');
CREATE INDEX routes_lookup ON routes (factory_id, source_key, status, updated_ms DESC);
CREATE INDEX routes_wi ON routes (work_item_id);
`,
};

interface RouteRow {
  work_item_id: string;
  work_token: string;
  status: string;
  kind: string;
  completed_ms: number | null;
  updated_ms: number;
}

/**
 * sourceKey names which of keys is the work item's source key. Senders set it
 * so that a route rebuilt from a status message keeps kind 'source'.
 */
type StatusPayload = OutboxPayloads["router.status"];

function tokenOf(wiId: string): string {
  return wiId.slice(wiId.indexOf("_") + 1, wiId.indexOf("_") + 17);
}

export class RouterDO extends BaseDO {
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.onMessage("router.status", (msg) => {
      const p = msg as StatusPayload;
      const now = Date.now();
      const status = p.stage === "complete" ? "completed" : p.stage === "cancelled" ? "cancelled" : "active";
      // Lower versions are ignored.
      this.sql.exec(
        `UPDATE routes SET status = ?, completed_ms = ?, wi_version = ?, updated_ms = ? WHERE work_item_id = ? AND wi_version < ?`,
        status,
        p.completedMs,
        p.version,
        now,
        p.wiId,
        p.version,
      );
      for (const key of p.keys) {
        if (
          first(
            this.sql,
            "SELECT 1 FROM routes WHERE factory_id = ? AND source_key = ? AND work_item_id = ?",
            p.factoryId,
            key,
            p.wiId,
          )
        )
          continue;
        // A missing route (a RouterDO reset, then a rebuild) gets the kind that
        // the normal path gives it: 'source' for the source key, so the key keeps
        // its owner. OR IGNORE keeps one active source route per key.
        this.sql.exec(
          `INSERT OR IGNORE INTO routes (factory_id, source_key, work_item_id, work_token, kind, status, completed_ms, wi_version, updated_ms)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
          p.factoryId,
          key,
          p.wiId,
          tokenOf(p.wiId),
          key === p.sourceKey ? "source" : "thread",
          status,
          p.completedMs,
          p.version,
          now,
        );
      }
    });
    this.onTimer("reserve_expiry", () => {
      this.sql.exec("DELETE FROM routes WHERE status = 'reserved' AND updated_ms < ?", Date.now() - RESERVE_TTL_MS);
      if (first(this.sql, "SELECT 1 FROM routes WHERE status = 'reserved' LIMIT 1")) {
        this.setTimer("reserve_expiry", "reserve_expiry", Date.now() + RESERVE_TTL_MS);
      }
    });
  }

  protected migrations(): readonly Migration[] {
    return [m0001];
  }

  async ping(): Promise<string> {
    return "RouterDO";
  }

  private lookup(factoryId: string, key: string): RouteRow | undefined {
    // Go order (runs.go findActiveWorkItem): active source route, then the newest active alias.
    return (
      first<RouteRow & Record<string, SqlStorageValue>>(
        this.sql,
        `SELECT work_item_id, work_token, status, kind, completed_ms, updated_ms FROM routes
         WHERE factory_id = ? AND source_key = ? AND status IN ('reserved', 'active')
         ORDER BY (kind = 'source') DESC, updated_ms DESC LIMIT 1`,
        factoryId,
        key,
      ) ??
      first<RouteRow & Record<string, SqlStorageValue>>(
        this.sql,
        `SELECT work_item_id, work_token, status, kind, completed_ms, updated_ms FROM routes
         WHERE factory_id = ? AND source_key = ? AND status = 'completed' AND completed_ms > ?
         ORDER BY completed_ms DESC LIMIT 1`,
        factoryId,
        key,
        Date.now() - REOPEN_WINDOW_MS,
      )
    );
  }

  async resolve(factoryId: string, sourceKey: string): Promise<{ wiId: string; token: string; status: string } | null> {
    return this.rpc(() => {
      const r = this.lookup(factoryId, sourceKey);
      return r ? { wiId: r.work_item_id, token: r.work_token, status: r.status } : null;
    });
  }

  /** Atomic: the active or reserved work item of any of the keys, or a new reservation on the first key. */
  async resolveOrReserve(input: {
    factoryId: string;
    sourceKeys: string[];
    actionKey: string;
    target: string;
  }): Promise<{ wiId: string; token: string; created: boolean }> {
    const token = await hashToken("w", this.teamId(), input.factoryId, input.actionKey, input.target);
    return this.rpc(() =>
      this.tx(() => {
        for (const key of input.sourceKeys) {
          const r = this.lookup(input.factoryId, key);
          if (r && r.status !== "completed") return { wiId: r.work_item_id, token: r.work_token, created: false };
        }
        const key = input.sourceKeys[0];
        const wiId = newRoutedId("wi", token);
        if (key) {
          this.sql.exec(
            `INSERT INTO routes (factory_id, source_key, work_item_id, work_token, kind, status, updated_ms) VALUES (?, ?, ?, ?, 'source', 'reserved', ?)`,
            input.factoryId,
            key,
            wiId,
            token,
            Date.now(),
          );
          this.timers.setEarliest("reserve_expiry", "reserve_expiry", Date.now() + RESERVE_TTL_MS);
        }
        return { wiId, token, created: true };
      }),
    );
  }

  /** link_pull_request / link_issue: claim a key for a work item (last writer wins for aliases). */
  async addReference(input: {
    factoryId: string;
    key: string;
    kind: string;
    wiId: string;
    token: string;
  }): Promise<{ ok: true } | { ok: false; ownerWiId: string }> {
    return this.rpc(() =>
      this.tx(() => {
        const src = first<{ work_item_id: string }>(
          this.sql,
          "SELECT work_item_id FROM routes WHERE factory_id = ? AND source_key = ? AND kind = 'source' AND status IN ('reserved', 'active')",
          input.factoryId,
          input.key,
        );
        if (src && src.work_item_id !== input.wiId) return { ok: false as const, ownerWiId: src.work_item_id };
        this.sql.exec(
          `INSERT INTO routes (factory_id, source_key, work_item_id, work_token, kind, status, updated_ms) VALUES (?, ?, ?, ?, ?, 'active', ?)
           ON CONFLICT(factory_id, source_key, work_item_id) DO UPDATE SET status = 'active', updated_ms = excluded.updated_ms`,
          input.factoryId,
          input.key,
          input.wiId,
          input.token,
          input.kind,
          Date.now(),
        );
        return { ok: true as const };
      }),
    );
  }

  /** reopen: no other active item for the key, then the completed route becomes active again. */
  async reopen(input: { factoryId: string; sourceKey: string; wiId: string }): Promise<boolean> {
    return this.rpc(() =>
      this.tx(() => {
        const other = first(
          this.sql,
          "SELECT 1 FROM routes WHERE factory_id = ? AND source_key = ? AND status IN ('reserved', 'active') AND work_item_id <> ?",
          input.factoryId,
          input.sourceKey,
          input.wiId,
        );
        if (other) return false;
        return (
          this.sql.exec(
            "UPDATE routes SET status = 'active', completed_ms = NULL, updated_ms = ? WHERE factory_id = ? AND source_key = ? AND work_item_id = ? AND status = 'completed'",
            Date.now(),
            input.factoryId,
            input.sourceKey,
            input.wiId,
          ).rowsWritten > 0
        );
      }),
    );
  }
}
