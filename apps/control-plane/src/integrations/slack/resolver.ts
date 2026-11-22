// Slack name resolver: channel names and user names or e-mail addresses to IDs (port of slack/resolver.go).
import { integrationActive } from "@openfactory/core";
import type { Integration } from "@openfactory/core";
import { NameLists, addId, sortIds, type NameMap } from "../namelist";
import type { IntegrationDeps, NameResolver } from "../types";
import { equalFold, integrationSecret } from "../util";
import { SlackApiError, SlackClient } from "./client";
import { PROVIDER } from "./responder";

const MAX_PAGES = 50;
const PAGE_SIZE = 200;

interface ListResponse {
  channels?: { id: string; name: string }[];
  members?: { id: string; name: string; deleted?: boolean; profile?: { email?: string } }[];
  response_metadata?: { next_cursor?: string };
}

function normalizeName(list: string, name: string): string {
  let n = name.trim();
  if (list === "channels") n = n.replace(/^#/, "").trim();
  return n.toLowerCase();
}

export class SlackNameResolver implements NameResolver {
  private readonly lists: NameLists;
  constructor(
    private readonly deps: IntegrationDeps,
    private readonly apiBaseUrl = "",
  ) {
    this.lists = new NameLists(deps.now ? () => deps.now!().getTime() : undefined);
  }

  /** kind is channels, users or itemUsers. A channel name ignores a leading "#". */
  async resolveNames(
    teamId: string,
    provider: string,
    kind: string,
    names: string[],
  ): Promise<Record<string, string[]>> {
    if (!equalFold(provider, PROVIDER)) throw new Error(`the Slack resolver cannot list ${provider} objects`);
    let list: string;
    switch (kind) {
      case "channels":
        list = "channels";
        break;
      case "users":
      case "itemUsers":
        list = "users";
        break;
      default:
        throw new Error(`the Slack resolver cannot resolve "${kind}"`);
    }
    let all: Integration[];
    try {
      all = (await this.deps.store.listIntegrations(teamId)).filter((i) => equalFold(i.provider, PROVIDER));
    } catch (e) {
      throw new Error(`could not read the Slack integrations: ${(e as Error).message}`, { cause: e });
    }
    const ints = all.filter(integrationActive);
    if (ints.length === 0) throw new Error("the team has no active Slack integration");
    const keys = names.map((n) => normalizeName(list, n));
    const out: NameMap = {};
    let firstErr: Error | undefined;
    let ok = 0;
    for (const in_ of ints) {
      let byName: NameMap;
      try {
        byName = await this.lists.lookup(`${in_.id}/${list}`, keys, () => this.fetchList(in_, list));
      } catch (e) {
        firstErr ??= e as Error;
        continue;
      }
      ok++;
      names.forEach((n, i) => {
        for (const id of byName[keys[i]!] ?? []) addId(out, n, id);
      });
    }
    if (ok === 0) throw firstErr!;
    sortIds(out);
    return out;
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
