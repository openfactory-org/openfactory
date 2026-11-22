// Base class of a plugin Worker. Import from "@openfactory/plugin-api/worker":
// it needs the Workers runtime ("cloudflare:workers").
//
// A plugin Worker exports `class Plugin extends PluginWorker`. Every public
// method of the class is an RPC method, so helpers must be module functions or
// #private members.

import { WorkerEntrypoint } from "cloudflare:workers";
import { PLUGIN_API_VERSION } from "./types";
import type { PluginBase, PluginInfo } from "./types";

export type PluginInfoInput = Omit<PluginInfo, "apiVersion" | "version"> & { version?: string };

export abstract class PluginWorker<E = unknown> extends WorkerEntrypoint<E> implements PluginBase {
  abstract info(): Promise<PluginInfo>;

  /** Plugin Worker vars and secrets are the settings; the default ignores the call. */
  async configure(_cfg: Record<string, string>): Promise<void> {}

  async ping(): Promise<void> {}
}

/** Fills the API version and the default version string. */
export function pluginInfo(input: PluginInfoInput, version?: string): PluginInfo {
  return { ...input, version: input.version ?? version ?? "0.0.0-dev", apiVersion: PLUGIN_API_VERSION };
}
