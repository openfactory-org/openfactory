import { ago, emit, table } from "../format.ts";
import { enc, explain, isJSON, makeClient, resolveFactory } from "../client.ts";
import { exactArgs, type Command } from "../framework.ts";

interface Webhook {
  id: string;
  name: string;
  auth_mode: string;
  signature_scheme?: string;
  enabled: boolean;
}

interface WebhookCreated {
  webhook: Webhook;
  ingress_url: string;
  secret?: string;
  awaiting_secret?: boolean;
}

interface Delivery {
  id: string;
  status: string;
  reason?: string;
  run_ids?: string[];
  received_at?: string;
}

const listCommand: Command = {
  use: "list <factory>",
  short: "List a factory's webhooks",
  args: exactArgs(1),
  async run({ args, globals, env }) {
    const c = makeClient(globals, env);
    const f = await resolveFactory(c, args[0] ?? "");
    const list = await c.list<Webhook>(`factories/${enc(f.uid)}/webhooks`).collect();
    if (isJSON(globals)) return emit(env.stdout, list);
    table(
      env.stdout,
      ["ID", "NAME", "AUTH", "SCHEME", "ENABLED"],
      list.map((w) => [w.id, w.name, w.auth_mode, w.signature_scheme ?? "", String(w.enabled)]),
    );
  },
};

const createCommand: Command = {
  use: "create <factory>",
  short: "Create a webhook on a managed factory",
  args: exactArgs(1),
  flags: [
    { name: "auth-mode", type: "string", default: "token", usage: "token, url_token or signature" },
    { name: "delivery-id-header", type: "string", usage: "header carrying a unique delivery id (deduplication)" },
    { name: "name", type: "string", usage: "webhook name (required)" },
    { name: "secret-name", type: "string", usage: "secret holding the credential (default derived from the name)" },
    {
      name: "signature-scheme",
      type: "string",
      usage: "github, pagerduty, sentry, standard_webhooks, stripe or vercel (auth-mode signature)",
    },
  ],
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const f = await resolveFactory(c, args[0] ?? "");
    const body: Record<string, string> = { name: flags.string("name") };
    const opt = (k: string, v: string): void => {
      if (v !== "") body[k] = v;
    };
    opt("auth_mode", flags.string("auth-mode"));
    opt("signature_scheme", flags.string("signature-scheme"));
    opt("secret_name", flags.string("secret-name"));
    opt("delivery_id_header", flags.string("delivery-id-header"));
    let res: WebhookCreated;
    try {
      res = await c.request<WebhookCreated>("POST", `factories/${enc(f.uid)}/webhooks`, { body });
    } catch (err) {
      throw explain(err);
    }
    if (isJSON(globals)) return emit(env.stdout, res);
    let out = `Created webhook ${res.webhook.name} (${res.webhook.id})\nIngress URL: ${res.ingress_url}\n`;
    if (res.secret) out += `Secret (shown once): ${res.secret}\n`;
    if (res.awaiting_secret)
      out += "Set the provider's signing secret with the dashboard or `webhook` API rotate-secret.\n";
    env.stdout.write(out);
  },
};

const deliveriesCommand: Command = {
  use: "deliveries <webhook-id>",
  short: "Show recent deliveries of a webhook",
  args: exactArgs(1),
  flags: [{ name: "limit", type: "int", default: 20, usage: "maximum deliveries" }],
  async run({ args, flags, globals, env }) {
    const c = makeClient(globals, env);
    const limit = flags.int("limit");
    const list = await c
      .list<Delivery>(`webhooks/${enc(args[0] ?? "")}/deliveries`, limit > 0 ? { limit } : {})
      .collect();
    if (isJSON(globals)) return emit(env.stdout, list);
    table(
      env.stdout,
      ["ID", "STATUS", "REASON", "RUNS", "RECEIVED"],
      list.map((d) => [d.id, d.status, d.reason ?? "", (d.run_ids ?? []).join(","), ago(d.received_at, env.now())]),
    );
  },
};

export const webhookCommand: Command = {
  use: "webhook",
  aliases: ["webhooks"],
  short: "Manage custom webhooks",
  subcommands: [listCommand, createCommand, deliveriesCommand],
};
