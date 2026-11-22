// TeamDO module: personal forge tokens. A user stores a token for a code
// forge; the forge resolver uses it for actions done as that user. The token
// is sealed with the team key. Migrations 0150-0199 (shared with schedules.ts
// and webhooks.ts).
import { OfError } from "@openfactory/core";
import { USER_TOKEN_FORGES, integrationProvider, supportsUserTokens } from "../../forges/registry";
import { bodyObject, str } from "../../lib/body";
import { timingSafeEqual } from "../../lib/bytes";
import type { Migration } from "../../lib/sql/migrate";
import { all, first } from "../../lib/sql/query";
import { noContent, type OpCall, type TeamKit, type TeamOp } from "./kit";
import * as secrets from "./secrets";

export const migrations: readonly Migration[] = [
  {
    version: 152,
    name: "user_forge_tokens",
    sql: `
CREATE TABLE user_forge_tokens (user_id TEXT NOT NULL, provider TEXT NOT NULL, envelope BLOB NOT NULL, updated_ms INTEGER NOT NULL,
  PRIMARY KEY (user_id, provider));
`,
  },
];

const tokenName = (userId: string, provider: string) => `user-forge-token/${userId}/${provider}`;

function userOf(c: OpCall): string {
  const p = c.principal();
  const uid = p.userId ?? (p.kind === "user" ? p.id : "");
  if (!uid) throw new OfError("not_authorized", "personal forge tokens require an API key that belongs to a user");
  return uid;
}

/** The decrypted personal token of a user for codeForge, or undefined. */
export async function getUserForgeToken(kit: TeamKit, userId: string, codeForge: string): Promise<string | undefined> {
  if (!userId) return undefined;
  const p = integrationProvider(codeForge);
  const r = first<{ envelope: ArrayBuffer }>(
    kit.sql,
    "SELECT envelope FROM user_forge_tokens WHERE user_id = ? AND provider = ?",
    userId,
    p,
  );
  if (!r) return undefined;
  const v = await secrets.openValue(
    kit,
    { teamId: kit.teamId(), factoryId: "", name: tokenName(userId, p), type: "generic" },
    new Uint8Array(r.envelope),
  );
  return new TextDecoder().decode(v);
}

async function setUserForgeToken(kit: TeamKit, c: OpCall): Promise<unknown> {
  const uid = userOf(c);
  const forge = (c.params["forge"] ?? "").toUpperCase();
  const tok = str(bodyObject(c.body, true), "token").trim();
  if (tok === "") throw new OfError("invalid_request", "token is required");
  if (!supportsUserTokens(forge)) {
    throw new OfError(
      "invalid_request",
      `${forge} does not support per-user tokens (supported: ${USER_TOKEN_FORGES.join(", ")})`,
    );
  }
  const p = integrationProvider(forge);
  const env = await secrets.sealValue(
    kit,
    { teamId: kit.teamId(), factoryId: "", name: tokenName(uid, p), type: "generic" },
    tok,
  );
  kit.tx(() =>
    kit.sql.exec(
      `INSERT INTO user_forge_tokens (user_id, provider, envelope, updated_ms) VALUES (?, ?, ?, ?)
       ON CONFLICT(user_id, provider) DO UPDATE SET envelope = excluded.envelope, updated_ms = excluded.updated_ms`,
      uid,
      p,
      env,
      Date.now(),
    ),
  );
  return noContent();
}

async function listUserForgeTokens(kit: TeamKit, c: OpCall): Promise<unknown> {
  const uid = userOf(c);
  const forges: string[] = [];
  const unreadable: string[] = [];
  for (const f of USER_TOKEN_FORGES) {
    const p = integrationProvider(f);
    if (!first(kit.sql, "SELECT 1 AS x FROM user_forge_tokens WHERE user_id = ? AND provider = ?", uid, p)) continue;
    forges.push(f);
    try {
      await getUserForgeToken(kit, uid, f);
    } catch {
      unreadable.push(f);
    }
  }
  return { forges, unreadable };
}

// ---------- ops ----------

export const ops: Record<string, TeamOp> = {
  listUserForgeTokens,
  setUserForgeToken,
  deleteUserForgeToken: (kit, c) => {
    const uid = userOf(c);
    const p = integrationProvider((c.params["forge"] ?? "").toUpperCase());
    kit.tx(() => kit.sql.exec("DELETE FROM user_forge_tokens WHERE user_id = ? AND provider = ?", uid, p));
    return noContent();
  },
  /** Internal (forge resolver): {token} of body {user_id, forge}. */
  userForgeTokenGet: async (kit, c) => {
    const b = bodyObject(c.body, true);
    const token = await getUserForgeToken(kit, str(b, "user_id"), str(b, "forge"));
    return { token: token ?? "" };
  },
};

/** The sealed personal forge tokens: POST /secrets/rewrap re-encrypts them. */
export const rewrapSources: readonly secrets.RewrapSource[] = [
  {
    name: "user_forge_tokens",
    list: (k) =>
      all<{ user_id: string; provider: string; envelope: ArrayBuffer }>(
        k.sql,
        "SELECT user_id, provider, envelope FROM user_forge_tokens",
      ).map((r) => ({
        key: `${r.user_id}\u0000${r.provider}`,
        id: { teamId: k.teamId(), factoryId: "", name: tokenName(r.user_id, r.provider), type: "generic" },
        envelope: new Uint8Array(r.envelope),
      })),
    replace: (k, key, old, next) => {
      const [uid, p] = key.split("\u0000") as [string, string];
      const cur = first<{ envelope: ArrayBuffer }>(
        k.sql,
        "SELECT envelope FROM user_forge_tokens WHERE user_id = ? AND provider = ?",
        uid,
        p,
      );
      if (!cur) return false;
      if (!timingSafeEqual(new Uint8Array(cur.envelope), old)) return false;
      k.sql.exec("UPDATE user_forge_tokens SET envelope = ? WHERE user_id = ? AND provider = ?", next, uid, p);
      return true;
    },
  },
];
