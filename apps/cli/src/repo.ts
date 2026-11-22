import { CliError } from "./framework.ts";

export interface RepoRef {
  codeForge: string;
  owner: string;
  name: string;
}

/**
 * Parses a repository entry: "forge:owner/name", "owner/name" (GitHub) or a
 * forge URL. Port of engine.ParseRepoSpec.
 */
export function parseRepoSpec(input: string): RepoRef {
  let s = input.trim();
  let forge = "GITHUB";
  const i = s.indexOf(":");
  if (i > 0 && !s.slice(0, i).includes("/")) {
    const f = s.slice(0, i).replaceAll("-", "_").toUpperCase();
    if (f === "HTTPS" || f === "HTTP") {
      try {
        const u = new URL(s);
        s = u.pathname.replace(/^\//, "");
        if (u.host.includes("gitlab")) forge = "GITLAB";
        else if (u.host.includes("dev.azure.com") || u.host.includes("visualstudio.com")) forge = "AZURE_DEVOPS";
      } catch {
        // keep s as is; the check below rejects it
      }
    } else {
      forge = f;
      s = s.slice(i + 1);
    }
  }
  s = s.replace(/^\/+|\/+$/g, "").replace(/\.git$/, "");
  const slash = s.lastIndexOf("/");
  if (slash <= 0 || slash === s.length - 1)
    throw new CliError(`invalid repository ${JSON.stringify(s)} (want forge:owner/name)`);
  return { codeForge: forge, owner: s.slice(0, slash), name: s.slice(slash + 1) };
}
