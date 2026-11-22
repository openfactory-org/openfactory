// Teams name resolver: channel names and user names or e-mail addresses to IDs, with Microsoft Graph (port of teams/resolver.go).
import { integrationActive } from "@openfactory/core";
import type { Integration } from "@openfactory/core";
import { NameLists, addId, addName, sortIds, type NameMap } from "../namelist";
import type { NameResolver } from "../types";
import { cfg, equalFold } from "../util";
import { str } from "./context";
import { INTEGRATION_PROVIDER, PROVIDER, describeGraphError, type TeamsHandler } from "./handler";

const inScope = (teams: string[] | undefined, id: string): boolean => !teams || teams.length === 0 || teams.some((t) => equalFold(t, id));

/**
 * The bot needs ChannelSettings.Read.Group (channels) and TeamMember.Read.Group
 * (users) in the team, and the record needs tenant_id.
 */
export class TeamsNameResolver implements NameResolver {
  private readonly lists: NameLists;
  constructor(private readonly h: TeamsHandler) {
    this.lists = new NameLists(h.deps.now ? () => h.deps.now!().getTime() : undefined);
  }

  async resolveNames(
    teamId: string,
    provider: string,
    kind: string,
    names: string[],
    scope: Record<string, string[]> = {},
  ): Promise<Record<string, string[]>> {
    if (!equalFold(provider, PROVIDER) && !equalFold(provider, INTEGRATION_PROVIDER)) {
      throw new Error(`the Teams resolver cannot list ${provider} objects`);
    }
    if (kind !== "channels" && kind !== "users") throw new Error(`the Teams resolver cannot resolve "${kind}"`);
    let all: Integration[];
    try {
      all = (await this.h.deps.store.listIntegrations(teamId)).filter((i) => equalFold(i.provider, INTEGRATION_PROVIDER));
    } catch (e) {
      throw new Error(`could not read the Teams integrations: ${(e as Error).message}`, { cause: e });
    }
    const ints = all.filter((i) => integrationActive(i) && cfg(i, "team_id") !== "" && inScope(scope["team_ids"], cfg(i, "team_id")));
    if (ints.length === 0) throw new Error("the team has no active Teams integration for these Microsoft teams");
    const keys = names.map((n) => {
      const k = n.trim().toLowerCase();
      return kind === "channels" ? k.replace(/^#/, "").trim() : k;
    });
    const out: NameMap = {};
    let firstErr: Error | undefined;
    let ok = 0;
    for (const in_ of ints) {
      let byName: NameMap;
      try {
        byName = await this.lookup(in_, kind, keys);
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

  private lookup(in_: Integration, kind: string, keys: string[]): Promise<NameMap> {
    const graphTeam = cfg(in_, "team_id");
    return this.lists.lookup(`${in_.id}/${kind}/${graphTeam}`, keys, async () => {
      const c = await this.h.graphClient(in_, "");
      const base = "/teams/" + graphTeam;
      const out: NameMap = {};
      if (kind === "channels") {
        try {
          await this.h.graphPages(c, base + "/channels?$select=id,displayName", (items) => {
            for (const ch of items) addName(out, str(ch, "displayName"), str(ch, "id"));
            return true;
          });
        } catch (e) {
          throw describeGraphError("list the channels", "ChannelSettings.Read.Group", e);
        }
        return out;
      }
      try {
        await this.h.graphPages(c, base + "/members?$top=999", (items) => {
          for (const m of items) {
            const id = str(m, "userId");
            addName(out, str(m, "displayName"), id);
            addName(out, str(m, "email"), id);
          }
          return true;
        });
      } catch (e) {
        throw describeGraphError("list the members", "TeamMember.Read.Group", e);
      }
      return out;
    });
  }
}
