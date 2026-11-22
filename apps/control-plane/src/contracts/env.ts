// Frozen contract (C0). The control-plane Worker environment: every binding,
// var and secret of wrangler.jsonc. Keep it in step with wrangler.jsonc.
// `npm run cf-types -w @openfactory/control-plane` writes the generated view to
// worker-configuration.d.ts (not compiled; review aid). test/env.test.ts checks
// that the runtime env has every name listed here.

import type { IngressPlugin, ForgePlugin } from "@openfactory/plugin-api";
import type { SandboxProviderRpc } from "@openfactory/sandbox";
import type { JobMessage } from "./do-messages";
import type { BenchmarkDO } from "../do/benchmark/index";
import type { DispatchDO } from "../do/dispatch/index";
import type { IndexDO } from "../do/index/index";
import type { IngressDO } from "../do/ingress/index";
import type { RouterDO } from "../do/router/index";
import type { TeamDO } from "../do/team/index";
import type { WorkDO } from "../do/work/index";
import type { WorkerHubDO } from "../do/workerhub/index";
import type * as MainModule from "../index";

/** A service binding typed with the RPC interface of its entrypoint. */
export type ServiceBinding<T> = Fetcher & T;

/** Plain configuration (wrangler.jsonc "vars"). All values are strings. */
export interface Vars {
  /** Public origin of the control plane, for example https://factory.example.com. */
  OF_PUBLIC_URL: string;
  /** Cloudflare account ID, for the R2 S3 endpoint used by presign and RequiredHosts. */
  OF_R2_ACCOUNT_ID: string;
  OF_R2_BUCKET: string;
  /** Duration, at most 7 days (R2 limit). */
  OF_OBJECT_STORE_PRESIGN_TTL: string;
  OF_QUEUE_TIMEOUT: string;
  OF_MAX_SESSION_DURATION: string;
  OF_SESSION_STOP_GRACE: string;
  OF_SANDBOX_START_TIMEOUT: string;
  /** Default sandbox provider for `workerHost: server`. */
  OF_SANDBOX_PROVIDER: string;
  /** "true" lets a restricted policy run on an egress "none" target. */
  OF_ALLOW_UNENFORCED_NETWORK_POLICY: string;
  /** "true" allows development process workers (`openfactory-runtime worker --process`). */
  OF_DEV_PROCESS_WORKERS: string;
  /** Version of openfactory-runtime that session start requires. */
  OF_RUNNER_VERSION: string;
  OF_OIDC_ISSUER_URL: string;
  OF_OIDC_CLIENT_ID: string;
  OF_LOG_LEVEL: string;
  /** Maximum depth of nested orchestration (runs started by runs). */
  OF_MAX_ORCHESTRATION_DEPTH?: string;
  /** Maximum sandboxes open at once for the team. */
  OF_MAX_SANDBOXES?: string;
}

/** Secrets (`wrangler secret put`). Required ones are listed in wrangler.jsonc. */
export interface Secrets {
  /** Envelope KEK for TeamDO secrets (AES-GCM). */
  OF_SECRET_KEY: string;
  /** Session cookies, run tokens, upload IDs. */
  OF_SIGNING_KEY: string;
  /** R2 API token (S3 credentials) for presigned URLs. */
  OF_R2_ACCESS_KEY_ID: string;
  OF_R2_SECRET_ACCESS_KEY: string;
  /** Rotation: the previous keys, still accepted for decryption and verification. */
  OF_SECRET_KEY_PREVIOUS?: string;
  OF_SIGNING_KEY_PREVIOUS?: string;
  OF_OIDC_CLIENT_SECRET?: string;
  /** P-256 private scalar of the run OIDC issuer (cloud identity). */
  OF_CLOUD_ID_KEY?: string;
}

export interface Bindings {
  // Durable Objects
  TEAM: DurableObjectNamespace<TeamDO>;
  WORK: DurableObjectNamespace<WorkDO>;
  DISPATCH: DurableObjectNamespace<DispatchDO>;
  ROUTER: DurableObjectNamespace<RouterDO>;
  INDEX: DurableObjectNamespace<IndexDO>;
  INGRESS: DurableObjectNamespace<IngressDO>;
  WORKER_HUB: DurableObjectNamespace<WorkerHubDO>;
  BENCHMARK: DurableObjectNamespace<BenchmarkDO>;
  // R2: snapshots, artifacts, attachments, transcripts, compacted messages, runner binaries
  DATA: R2Bucket;
  // Queue producer: unordered fan-out jobs
  JOBS: Queue<JobMessage>;
  // Dashboard (Static Assets)
  ASSETS: Fetcher;
  // Service bindings (typed RPC)
  PROVIDER_CLOUDFLARE_SANDBOX: ServiceBinding<SandboxProviderRpc>;
  PLUGIN_GITLAB: ServiceBinding<ForgePlugin & IngressPlugin>;
  PLUGIN_AZUREDEVOPS: ServiceBinding<ForgePlugin & IngressPlugin>;
}

export interface Env extends Bindings, Vars, Secrets {}

/** Every var name, in wrangler.jsonc order. */
export const VAR_NAMES = [
  "OF_PUBLIC_URL",
  "OF_R2_ACCOUNT_ID",
  "OF_R2_BUCKET",
  "OF_OBJECT_STORE_PRESIGN_TTL",
  "OF_QUEUE_TIMEOUT",
  "OF_MAX_SESSION_DURATION",
  "OF_SESSION_STOP_GRACE",
  "OF_SANDBOX_START_TIMEOUT",
  "OF_SANDBOX_PROVIDER",
  "OF_ALLOW_UNENFORCED_NETWORK_POLICY",
  "OF_DEV_PROCESS_WORKERS",
  "OF_RUNNER_VERSION",
  "OF_OIDC_ISSUER_URL",
  "OF_OIDC_CLIENT_ID",
  "OF_LOG_LEVEL",
  "OF_MAX_ORCHESTRATION_DEPTH",
  "OF_MAX_SANDBOXES",
] as const satisfies readonly (keyof Vars)[];

/** Required secrets (wrangler.jsonc secrets.required). */
export const REQUIRED_SECRET_NAMES = [
  "OF_SECRET_KEY",
  "OF_SIGNING_KEY",
  "OF_R2_ACCESS_KEY_ID",
  "OF_R2_SECRET_ACCESS_KEY",
] as const satisfies readonly (keyof Secrets)[];

// Compile-time check: VAR_NAMES lists every key of Vars.
type MissingVars = Exclude<keyof Vars, (typeof VAR_NAMES)[number]>;
export const VAR_NAMES_COMPLETE: [MissingVars] extends [never] ? true : MissingVars = true;

/** Names of the DO bindings, in wrangler.jsonc order. */
export const DO_BINDINGS = {
  TEAM: "TeamDO",
  WORK: "WorkDO",
  DISPATCH: "DispatchDO",
  ROUTER: "RouterDO",
  INDEX: "IndexDO",
  INGRESS: "IngressDO",
  WORKER_HUB: "WorkerHubDO",
  BENCHMARK: "BenchmarkDO",
} as const satisfies Partial<Record<keyof Bindings, string>>;

declare global {
  // Makes `env` from "cloudflare:workers" and "cloudflare:test" typed.
  // eslint-disable-next-line @typescript-eslint/no-namespace
  namespace Cloudflare {
    interface Env extends Bindings, Vars, Secrets {}
    interface GlobalProps {
      mainModule: typeof MainModule;
      durableNamespaces: (typeof DO_BINDINGS)[keyof typeof DO_BINDINGS];
    }
  }
}
