// Teams name resolver: channel names and user names or e-mail addresses to IDs, with Microsoft Graph (port of teams/resolver.go).
import type { Integration } from "@openfactory/core";
import { addName, type NameMap } from "../namelist";
import { BaseNameResolver } from "../resolver";
import { cfg, equalFold } from "../util";
import { str } from "./context";
import { INTEGRATION_PROVIDER, PROVIDER, describeGraphError, type TeamsHandler } from "./handler";

const inScope = (teams: string[] | undefined, id: string): boolean =>
  !teams || teams.length === 0 || teams.some((t) => equalFold(t, id));

/**
 * The bot needs ChannelSettings.Read.Group (channels) and TeamMember.Read.Group
 * (users) in the team, and the record needs tenant_id.
 */
export class TeamsNameResolver extends BaseNameResolver {
  constructor(private readonly h: TeamsHandler) {
    super(h.deps, {
      label: "Teams",
      providers: [PROVIDER, INTEGRATION_PROVIDER],
      integrationProvider: INTEGRATION_PROVIDER,
      kinds: ["channels", "users"],
    });
  }

  /** A channel name ignores a leading "#". */
  protected override normalize(kind: string, name: string): string {
    const k = name.trim().toLowerCase();
    return kind === "channels" ? k.replace(/^#/, "").trim() : k;
  }

  /** The record must name a Microsoft team, and that team must be in scope["team_ids"] when it is set. */
  protected override usable(in_: Integration, scope: Record<string, string[]>): boolean {
    return cfg(in_, "team_id") !== "" && inScope(scope["team_ids"], cfg(in_, "team_id"));
  }

  protected override noIntegrationMessage(): string {
    return "the team has no active Teams integration for these Microsoft teams";
  }

  protected override lookup(in_: Integration, kind: string, keys: string[]): Promise<NameMap> {
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
