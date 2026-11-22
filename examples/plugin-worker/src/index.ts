// Example Open Factory plugin Worker. It serves two roles:
//
//   - responder for the "hello" provider: logs every outbound message;
//   - ingress at POST /plugins/hello/webhook: turns the JSON body
//     {"teamId": "...", "factoryId": "...", "text": "..."} into one inbound
//     event of provider "hello", type "received".
//
// The control plane binds this Worker as a service binding named PLUGIN_HELLO
// with entrypoint "Plugin" and calls the public methods below with typed RPC.

import { PLUGIN_API_VERSION, rpc } from "@openfactory/plugin-api";
import type {
  HostApi,
  IngressPlugin,
  IssueState,
  OutboundMessage,
  PluginEvent,
  PluginInfo,
  PullRequestRef,
  ResponderPlugin,
  SourceRef,
  WebhookResult,
} from "@openfactory/plugin-api";
import { PluginWorker } from "@openfactory/plugin-api/worker";

const PROVIDER = "hello";
const MAX_BODY_BYTES = 1 << 20;

interface Env {
  /** Optional Worker var. */
  GREETING?: string;
}

function reject(status: number, message: string): WebhookResult {
  return { status, headers: { "content-type": "application/json" }, body: JSON.stringify({ error: message }), events: [] };
}

export class Plugin extends PluginWorker<Env> implements ResponderPlugin, IngressPlugin {
  async info(): Promise<PluginInfo> {
    return {
      name: "hello",
      version: "0.1.0",
      // Always the constant of the plugin API you built against.
      apiVersion: PLUGIN_API_VERSION,
      roles: ["responder", "ingress"],
      responderProviders: [PROVIDER],
      integrationProviders: [PROVIDER],
    };
  }

  /** Logs the message instead of posting it anywhere. */
  reply(provider: string, teamId: string, message: OutboundMessage): Promise<void> {
    return rpc(() => {
      console.log(JSON.stringify({ msg: this.env.GREETING ?? "hello reply", provider, teamId, kind: message.kind, text: message.text }));
    });
  }

  setIssueState(_provider: string, _teamId: string, _issue: SourceRef, _state: IssueState): Promise<void> {
    // Optional methods that a plugin does not support are no-ops here.
    return Promise.resolve();
  }

  attachPullRequest(_provider: string, _teamId: string, _issue: SourceRef, _pr: PullRequestRef): Promise<void> {
    return Promise.resolve();
  }

  /** The control plane forwards the raw request. `events` go to IngressDO, the rest to the sender. */
  handleWebhook(req: Request, host: HostApi): Promise<WebhookResult> {
    return rpc(async () => {
      const raw = await req.text();
      if (raw.length > MAX_BODY_BYTES) return reject(413, "payload too large");
      let body: { teamId?: string; factoryId?: string; text?: string };
      try {
        body = JSON.parse(raw) as typeof body;
      } catch {
        return reject(400, "invalid JSON body");
      }
      if (!body.teamId || !body.text) return reject(400, "teamId and text are required");
      const delivery = req.headers.get("x-delivery-id") ?? crypto.randomUUID();
      const event: PluginEvent = {
        id: `evt_${crypto.randomUUID().replaceAll("-", "")}`,
        teamId: body.teamId,
        ...(body.factoryId ? { factoryId: body.factoryId } : {}),
        provider: PROVIDER,
        type: "received",
        deliveryId: delivery,
        actor: { provider: PROVIDER, externalId: "webhook", handle: "webhook", isBot: false },
        source: { provider: PROVIDER, kind: "message", externalId: delivery, title: body.text.slice(0, 80) },
        attributes: { text: body.text },
        text: body.text,
        receivedAt: new Date().toISOString(),
      };
      await host.log("info", "hello webhook", { team: body.teamId });
      return {
        status: 202,
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ status: "accepted", events: 1 }),
        events: [event],
      };
    });
  }
}

export default {
  fetch(): Response {
    return new Response("hello-plugin: call this Worker through a service binding", { status: 404 });
  },
};
