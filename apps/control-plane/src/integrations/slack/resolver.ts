// Slack name resolver: channel names and user names or e-mail addresses to IDs (port of slack/resolver.go).
import type { Integration } from "@openfactory/core";
import { addId, type NameMap } from "../namelist";
import { BaseNameResolver } from "../resolver";
import type { IntegrationDeps } from "../types";
import { integrationSecret } from "../util";
import { SlackApiError, SlackClient } from "./client";
import { PROVIDER } from "./responder";

const MAX_PAGES = 50;
const PAGE_SIZE = 200;

interface ListResponse {
  channels?: { id: string; name: string }[];
  members?: { id: string; name: string; deleted?: boolean; profile?: { email?: string } }[];
  response_metadata?: { next_cursor?: string };
}

export class SlackNameResolver extends BaseNameResolver {
  constructor(
    deps: IntegrationDeps,
    private readonly apiBaseUrl = "",
  ) {
    super(deps, {
      label: "Slack",
      providers: [PROVIDER],
      integrationProvider: PROVIDER,
      kinds: ["channels", "users", "itemUsers"],
    });
  }

  /** A channel name ignores a leading "#". */
  protected override normalize(kind: string, name: string): string {
    let n = name.trim();
    if (kind === "channels") n = n.replace(/^#/, "").trim();
    return n.toLowerCase();
  }

  /** kind is channels, users or itemUsers; itemUsers reads the user list. */
  protected override lookup(in_: Integration, kind: string, keys: string[]): Promise<NameMap> {
    const list = kind === "channels" ? "channels" : "users";
    return this.lists.lookup(`${in_.id}/${list}`, keys, () => this.fetchList(in_, list));
  }

  private async fetchList(in_: Integration, kind: string): Promise<NameMap> {
    try {
      const tok = await integrationSecret(this.deps.secrets, in_);
      const c = new SlackClient(this.apiBaseUrl, tok, this.deps.fetch);
      return kind === "channels" ? await listChannels(c) : await listUsers(c);
    } catch (e) {
      throw describeListError(kind, e);
    }
  }
}

function describeListError(kind: string, e: unknown): unknown {
  if (e instanceof SlackApiError) {
    switch (e.code) {
      case "missing_scope": {
        const scopes = kind === "users" ? "users:read and users:read.email" : "channels:read and groups:read";
        return new Error(`the Slack app needs the scopes ${scopes} to list ${kind}: ${e.message}`, { cause: e });
      }
      case "invalid_auth":
      case "token_revoked":
      case "account_inactive":
      case "not_authed":
        return new Error(`Slack refused the bot token: ${e.message}`, { cause: e });
    }
  }
  return e;
}

/** The public and private channels that the bot can see. Without the scope for private channels, the public ones. */
async function listChannels(c: SlackClient): Promise<NameMap> {
  try {
    return await pageChannels(c, "public_channel,private_channel");
  } catch (e) {
    if (e instanceof SlackApiError && e.code === "missing_scope") return pageChannels(c, "public_channel");
    throw e;
  }
}

async function pageChannels(c: SlackClient, types: string): Promise<NameMap> {
  const out: NameMap = {};
  let cursor = "";
  for (let page = 0; page < MAX_PAGES; page++) {
    const q = new URLSearchParams({ types, exclude_archived: "true", limit: String(PAGE_SIZE) });
    if (cursor !== "") q.set("cursor", cursor);
    const resp = await c.call<ListResponse>("conversations.list", q);
    for (const ch of resp.channels ?? []) (out[ch.name.toLowerCase()] ??= []).push(ch.id);
    cursor = resp.response_metadata?.next_cursor ?? "";
    if (cursor === "") break;
  }
  return out;
}

/** The users by username and e-mail address. Deleted users are left out. */
async function listUsers(c: SlackClient): Promise<NameMap> {
  const out: NameMap = {};
  const add = (k: string, id: string): void => {
    const key = k.trim().toLowerCase();
    if (key !== "") addId(out, key, id);
  };
  let cursor = "";
  for (let page = 0; page < MAX_PAGES; page++) {
    const q = new URLSearchParams({ limit: String(PAGE_SIZE) });
    if (cursor !== "") q.set("cursor", cursor);
    const resp = await c.call<ListResponse>("users.list", q);
    for (const m of resp.members ?? []) {
      if (m.deleted) continue;
      add(m.name, m.id);
      add(m.profile?.email ?? "", m.id);
    }
    cursor = resp.response_metadata?.next_cursor ?? "";
    if (cursor === "") break;
  }
  return out;
}
