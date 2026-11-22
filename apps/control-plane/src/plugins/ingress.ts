// POST /plugins/{name}/webhook: forwards the raw request to the plugin Worker,
// returns the plugin's response to the sender and hands the events to IngressDO.
// Port of internal/plugin/ingress.go.

import { OfError, newId, problemResponse, toOfError } from "@openfactory/core";
import type { HostApi, PluginEvent, WebhookResult } from "@openfactory/plugin-api";
import type { CoreEvent } from "../contracts/do-messages";
import { eventDedupeKey, eventToCore } from "./mappers";
import type { PluginRegistry } from "./registry";

/** One event for IngressDO: the partition is chosen by the caller from the event. */
export interface PluginDelivery {
  event: CoreEvent;
  dedupeKey: string;
}

export interface WebhookForwardOptions {
  registry: PluginRegistry;
  /** Builds the host for the plugin. Webhooks have no team, so the host is not team-scoped. */
  host: (pluginName: string) => HostApi;
  /** Hands the events to IngressDO (ingress.deliver). An error makes the sender retry. */
  deliver: (deliveries: PluginDelivery[]) => Promise<void>;
  now?: () => Date;
  log?: (message: string, fields: Record<string, unknown>) => void;
}

/** Response headers that a plugin may set. */
const ALLOWED_HEADERS = new Set(["content-type", "www-authenticate", "retry-after", "cache-control"]);

export function webhookResponse(r: Pick<WebhookResult, "status" | "headers" | "body">): Response {
  const status = Number.isInteger(r.status) && r.status >= 200 && r.status <= 599 ? r.status : 502;
  const headers = new Headers();
  for (const [k, v] of Object.entries(r.headers ?? {})) {
    if (ALLOWED_HEADERS.has(k.toLowerCase())) headers.set(k, v);
  }
  // A null-body status has no body.
  const body = status === 204 || status === 205 || status === 304 ? null : (r.body ?? "");
  return new Response(body, { status, headers });
}

/** The path name of /plugins/{name}/webhook, or undefined for another path. */
export function pluginWebhookName(pathname: string): string | undefined {
  const m = /^\/plugins\/([a-z0-9_-]+)\/webhook\/?$/.exec(pathname);
  return m?.[1];
}

/**
 * Forwards one webhook. The events of a result without a team are dropped, since
 * the engine routes by team. The Response is the plugin's answer; failures of the
 * plugin or of the delivery are problem responses.
 */
export async function forwardPluginWebhook(name: string, req: Request, opts: WebhookForwardOptions): Promise<Response> {
  const log = opts.log ?? ((message, fields) => console.warn(JSON.stringify({ message, ...fields })));
  if (req.method !== "POST") {
    return problemResponse(new OfError("invalid_request", "plugin webhooks accept POST only"), {
      instance: new URL(req.url).pathname,
    });
  }
  const plugin = opts.registry.get(name);
  if (!plugin || !plugin.info.roles.includes("ingress") || !plugin.stub.handleWebhook) {
    return problemResponse(new OfError("resource_not_found", `plugin ${name} serves no webhook`), {
      instance: new URL(req.url).pathname,
    });
  }
  let result: WebhookResult;
  try {
    result = await plugin.stub.handleWebhook(req, opts.host(plugin.info.name));
  } catch (e) {
    const { error } = toOfError(e);
    log("plugin webhook failed", { plugin: name, code: error.code, error: error.message });
    // A failed plugin is unavailable to the sender, which then retries.
    const out = error.code === "internal_error" ? new OfError("resource_unavailable", `plugin ${name} failed`) : error;
    return problemResponse(out, { instance: new URL(req.url).pathname });
  }
  const deliveries = toDeliveries(result.events ?? [], plugin.info.name, opts.now, log);
  if (deliveries.length > 0) {
    try {
      await opts.deliver(deliveries);
    } catch (e) {
      const { error } = toOfError(e);
      log("plugin webhook delivery failed", { plugin: name, code: error.code, error: error.message });
      return problemResponse(new OfError("resource_unavailable", "the events could not be stored; retry"), {
        instance: new URL(req.url).pathname,
      });
    }
  }
  return webhookResponse(result);
}

/** Validates the events of a plugin and converts them to core events. */
export function toDeliveries(
  events: PluginEvent[],
  plugin: string,
  now?: () => Date,
  log: (message: string, fields: Record<string, unknown>) => void = () => {},
): PluginDelivery[] {
  const out: PluginDelivery[] = [];
  for (const ev of events) {
    if (!ev.teamId || !ev.provider || !ev.type) {
      log("plugin event dropped", { plugin, reason: "an event needs teamId, provider and type", type: ev.type });
      continue;
    }
    const id = ev.id && ev.id !== "" ? ev.id : newId("evt");
    out.push({ event: eventToCore(ev, id, now), dedupeKey: eventDedupeKey({ ...ev, id }) });
  }
  return out;
}
