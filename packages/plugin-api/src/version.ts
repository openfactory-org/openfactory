import { PLUGIN_API_VERSION } from "./types";
import type { PluginInfo } from "./types";

const NAME_RE = /^[a-z0-9_-]+$/;

export type VersionCheck = { ok: true } | { ok: false; reason: string };

/** Checks the info of a plugin against the API version of this host. */
export function checkPluginInfo(info: PluginInfo, hostVersion: number = PLUGIN_API_VERSION): VersionCheck {
  if (!NAME_RE.test(info.name)) return { ok: false, reason: `invalid plugin name ${JSON.stringify(info.name)}` };
  if (info.apiVersion !== hostVersion) {
    return {
      ok: false,
      reason: `plugin ${info.name} speaks plugin API version ${info.apiVersion}; this control plane speaks ${hostVersion}`,
    };
  }
  if (!Array.isArray(info.roles) || info.roles.length === 0) {
    return { ok: false, reason: `plugin ${info.name} serves no role` };
  }
  return { ok: true };
}
