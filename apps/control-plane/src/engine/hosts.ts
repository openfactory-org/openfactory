// RequiredHosts (internal/engine/required_hosts.go) without Bedrock and
// image registries, plus the R2 S3 host (plan: "R2 must be reachable"). Pure.
import type { AgentConfig } from "./types";

const FORGE_HOSTS: Record<string, string[]> = {
  GITHUB: ["github.com", "api.github.com", "codeload.github.com"],
  GITLAB: ["gitlab.com"],
  AZURE_DEVOPS: ["dev.azure.com"],
};

export function hostOf(raw: string | undefined): string {
  if (!raw) return "";
  try {
    return new URL(raw).hostname.toLowerCase().replace(/^\[|\]$/g, "");
  } catch {
    return "";
  }
}

export function r2Host(accountId: string): string {
  return accountId ? `${accountId.toLowerCase()}.r2.cloudflarestorage.com` : "";
}

export function inferenceHosts(harness: { type?: string } | undefined, auth: Record<string, string> = {}): string[] {
  switch (harness?.type) {
    case "claude":
    case "claude-code":
      return [hostOf(auth.ANTHROPIC_BASE_URL) || "api.anthropic.com"];
    case "codex":
      return [hostOf(auth.OPENAI_BASE_URL) || "api.openai.com"];
  }
  return [];
}

export interface RequiredHostsOptions {
  /** OF_CONTROL_URL of the task (the public control-plane URL). */
  controlUrl: string;
  r2AccountId: string;
  repos?: { clone_url?: string; forge?: string }[];
  mcpServerUrls?: string[];
  auth?: Record<string, string>;
  gcp?: boolean;
}

export function requiredHosts(cfg: Pick<AgentConfig, "harness">, o: RequiredHostsOptions): string[] {
  const hosts: string[] = [];
  const add = (...hs: string[]) => {
    for (const h of hs) {
      const v = h.trim().toLowerCase();
      if (v && !hosts.includes(v)) hosts.push(v);
    }
  };
  add(hostOf(o.controlUrl));
  add(r2Host(o.r2AccountId));
  for (const co of o.repos ?? []) {
    add(hostOf(co.clone_url));
    const pub = FORGE_HOSTS[(co.forge ?? "").toUpperCase()];
    if (pub && (!co.clone_url || hostOf(co.clone_url) === pub[0] || co.forge === "AZURE_DEVOPS")) add(...pub);
  }
  for (const u of o.mcpServerUrls ?? []) add(hostOf(u));
  add(...inferenceHosts(cfg.harness, o.auth));
  if (o.gcp) add("sts.googleapis.com", "iamcredentials.googleapis.com");
  return hosts;
}
