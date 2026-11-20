import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { fpath, useFactory } from "../components/FactoryLayout";
import { Plus, Webhook as WebhookIcon } from "lucide-react";
import { confirm, Dialog } from "../components/overlays";
import { Alert, Badge, Button, Card, Code, Empty, ErrorAlert, Field, KV, PageHeader, Query, TimeAgo } from "../components/ui";
import { fmtTime } from "../lib/format";
import { useMutate } from "../lib/mutate";
import { ReadOnlyNotice } from "./Agents";

type Webhook = Schemas["Webhook"];
type Delivery = Schemas["Delivery"];
type TestFilterResult = Schemas["TestFilterResult"];

const TONES_BY_STATUS: Record<string, string> = { accepted: "ok", rejected: "bad", deduplicated: "neutral", duplicate: "neutral" };

const SCHEMES = ["github", "pagerduty", "sentry", "standard_webhooks", "stripe", "vercel"] as const;

export function CopyField({ id, label, value }: { id: string; label: string; value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      <div className="input-row">
        <input id={id} readOnly value={value} className="mono" onFocus={(e) => e.target.select()} />
        <Button
          onClick={() => {
            void navigator.clipboard?.writeText(value).then(() => {
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            });
          }}
        >
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </div>
  );
}

const HEX_SHA256 = `SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')`;

/** What a sender adds to a delivery, per signature scheme (internal/webhooks/signature.go). */
const SIGNATURES: Record<string, { header: string; how: string; sign: string[] }> = {
  github: {
    header: "X-Hub-Signature-256: sha256=<signature>",
    how: "The signature is the HMAC-SHA256 of the raw body with the secret, in hex.",
    sign: [HEX_SHA256],
  },
  pagerduty: {
    header: "X-PagerDuty-Signature: v1=<signature>",
    how: "The signature is the HMAC-SHA256 of the raw body with the secret, in hex.",
    sign: [HEX_SHA256],
  },
  sentry: {
    header: "Sentry-Hook-Signature: <signature>",
    how: "The signature is the HMAC-SHA256 of the raw body with the secret, in hex.",
    sign: [HEX_SHA256],
  },
  vercel: {
    header: "X-Vercel-Signature: <signature>",
    how: "The signature is the HMAC-SHA1 of the raw body with the secret, in hex.",
    sign: [`SIG=$(printf '%s' "$BODY" | openssl dgst -sha1 -hmac "$SECRET" -hex | sed 's/^.* //')`],
  },
  stripe: {
    header: "Stripe-Signature: t=<timestamp>,v1=<signature>",
    how: 'The signature is the HMAC-SHA256 of "<timestamp>.<raw body>" with the secret, in hex. The timestamp is in Unix seconds and must be recent.',
    sign: ["TS=$(date +%s)", `SIG=$(printf '%s' "$TS.$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')`],
  },
  standard_webhooks: {
    header: "Webhook-Id: <id>, Webhook-Timestamp: <timestamp> and Webhook-Signature: v1,<signature>",
    how: 'The signature is the HMAC-SHA256 of "<id>.<timestamp>.<raw body>", in base64. The key is the secret without "whsec_", base64-decoded. The timestamp is in Unix seconds and must be recent.',
    sign: [
      "ID=msg_$(date +%s)",
      "TS=$(date +%s)",
      `KEY=$(printf '%s' "\${SECRET#whsec_}" | base64 -d | od -An -tx1 | tr -d ' \\n')`,
      `SIG=$(printf '%s' "$ID.$TS.$BODY" | openssl dgst -sha256 -mac HMAC -macopt hexkey:$KEY -binary | base64)`,
    ],
  },
};

type UsageHook = Pick<Webhook, "auth_mode" | "signature_scheme" | "ingress_url">;

/** How to call a webhook: what to send, and a curl example with a placeholder credential. */
export function webhookUsage(h: UsageHook): { lines: string[]; curl: string } {
  const base = (h.ingress_url ?? "").replace(/\/\{token\}$/, "");
  const body = `BODY='{"event":"test"}'`;
  const json = `  -H "Content-Type: application/json" \\`;
  if (h.auth_mode === "url_token") {
    return {
      lines: ["Put the token at the end of the URL: POST <ingress URL>/<token>. No credential header is needed."],
      curl: [`TOKEN='<token>'`, body, `curl -X POST "${base}/$TOKEN" \\`, json, `  -d "$BODY"`].join("\n"),
    };
  }
  if (h.auth_mode === "signature") {
    const sc = SIGNATURES[h.signature_scheme ?? ""];
    if (!sc) return { lines: ["The provider signs each delivery."], curl: "" };
    const headers =
      h.signature_scheme === "standard_webhooks"
        ? [`  -H "Webhook-Id: $ID" \\`, `  -H "Webhook-Timestamp: $TS" \\`, `  -H "Webhook-Signature: v1,$SIG" \\`]
        : [`  -H "${sc.header.replace("<signature>", "$SIG").replace("<timestamp>", "$TS")}" \\`];
    return {
      lines: [`The provider signs each delivery with the header ${sc.header}.`, sc.how],
      curl: [`SECRET='<secret>'`, body, ...sc.sign, `curl -X POST "${base}" \\`, json, ...headers, `  -d "$BODY"`].join("\n"),
    };
  }
  return {
    lines: ["Send the token in the header Authorization: Bearer <token>."],
    curl: [`TOKEN='<token>'`, body, `curl -X POST "${base}" \\`, `  -H "Authorization: Bearer $TOKEN" \\`, json, `  -d "$BODY"`].join("\n"),
  };
}

function CopyButton({ value, label = "Copy" }: { value: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <Button
      onClick={() => {
        void navigator.clipboard?.writeText(value).then(() => {
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        });
      }}
    >
      {copied ? "Copied" : label}
    </Button>
  );
}

/** Explains how to send the credential of a webhook, with a curl example. */
export function WebhookUsage({ h }: { h: UsageHook }) {
  const u = webhookUsage(h);
  return (
    <div className="stack stack-sm" role="group" aria-label="How to call this webhook">
      <strong>How to call it</strong>
      {u.lines.map((l) => (
        <p key={l} className="small">
          {l}
        </p>
      ))}
      {u.curl && (
        <>
          <Code>{u.curl}</Code>
          <div>
            <CopyButton value={u.curl} label="Copy curl example" />
          </div>
        </>
      )}
    </div>
  );
}

function useWebhooks(fid: string) {
  return useQuery({
    queryKey: ["webhooks", fid],
    queryFn: () => unwrap(api.GET("/factories/{uid}/webhooks", { params: { path: { uid: fid } } })).then((r) => (r.items ?? []) as Webhook[]),
  });
}

export function WebhooksPage() {
  const f = useFactory();
  const { canEditFactory } = useAuth();
  const q = useWebhooks(f.id);
  const editable = canEditFactory(f.id) && f.source !== "git";
  const [form, setForm] = useState({ name: "", auth_mode: "token", signature_scheme: "", secret: "", secret_name: "", delivery_id_header: "" });
  const [created, setCreated] = useState<Schemas["WebhookCreated"] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [open, setOpen] = useState(false);
  const create = useMutate(
    (body: Schemas["CreateWebhookRequest"]) => unwrap(api.POST("/factories/{uid}/webhooks", { params: { path: { uid: f.id } }, body })),
    {
      invalidate: [["webhooks", f.id], ["definition", f.id]],
      quietErrors: true,
      success: (r) => `Webhook ${r.webhook.name} created.`,
      onSuccess: (r) => {
        setOpen(false);
        setCreated(r);
        setForm({ name: "", auth_mode: "token", signature_scheme: "", secret: "", secret_name: "", delivery_id_header: "" });
      },
    },
  );

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    try {
      await create.mutateAsync({
        name: form.name.trim(),
        auth_mode: form.auth_mode as Schemas["CreateWebhookRequest"]["auth_mode"],
        signature_scheme: form.auth_mode === "signature" ? form.signature_scheme || undefined : undefined,
        secret: form.auth_mode === "signature" ? form.secret || undefined : undefined,
        secret_name: form.secret_name.trim() || undefined,
        delivery_id_header: form.delivery_id_header.trim() || undefined,
      });
    } catch (err) {
      setError(err);
    }
  }

  return (
    <>
      <PageHeader
        title="Webhooks"
        crumbs={[{ label: f.name, to: fpath(f) }]}
        subtitle="Authenticated URLs any system can POST JSON to. Automations subscribe with a webhook trigger."
        actions={
          <>
            <Link className="btn btn-ghost hide-sm" to={fpath(f, "automations")}>
              Automations
            </Link>
            {editable && (
              <Button variant="primary" onClick={() => setOpen(true)}>
                <Plus size={14} aria-hidden="true" /> Add webhook
              </Button>
            )}
          </>
        }
      />
      <ReadOnlyNotice f={f} />
      {created && (
        <section className="card created" role="status" aria-label="Webhook created">
          <h2 className="card-title">Webhook created</h2>
          <p>
            Copy these now.{" "}
            {created.secret && (
              <>
                The secret is not shown again; it is stored in the managed secret <code>{created.webhook.secret_name}</code>.
              </>
            )}
            {created.secret_reused && (
              <>
                The webhook uses the existing team secret <code>{created.webhook.secret_name}</code>; its value is not shown. Use the value you stored there, or rotate the secret on the webhook page to get a new one.
              </>
            )}
          </p>
          <CopyField id="c-url" label="Ingress URL" value={created.ingress_url} />
          {created.secret && <CopyField id="c-secret" label="Secret" value={created.secret} />}
          {created.awaiting_secret && (
            <Alert kind="warning">
              The webhook was created disabled. Give the provider the URL above, then store the signing secret it issues in <code>{created.webhook.secret_name}</code> on the{" "}
              <Link to="/secrets">Secrets</Link> page and enable the webhook.
            </Alert>
          )}
          <WebhookUsage h={created.webhook} />
          <p className="small">
            Webhook UID: <code>{created.webhook.id}</code> — use it in an automation trigger's <code>webhook_ids</code>.
          </p>
        </section>
      )}
      <Query q={q}>
        {(hooks) =>
          hooks.length === 0 ? (
            <Empty
              icon={<WebhookIcon size={18} />}
              title="No webhooks yet"
              action={
                editable ? (
                  <Button variant="primary" onClick={() => setOpen(true)}>
                    Add webhook
                  </Button>
                ) : undefined
              }
            >
              A webhook gives another system a URL that starts work in this factory.
            </Empty>
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Authentication</th>
                    <th scope="col">Status</th>
                    <th scope="col">Ingress URL</th>
                  </tr>
                </thead>
                <tbody>
                  {hooks.map((h) => (
                    <tr key={h.id}>
                      <td>
                        <Link to={fpath(f, `webhooks/${encodeURIComponent(h.id)}`)}>{h.name}</Link>
                      </td>
                      <td>
                        {h.auth_mode}
                        {h.signature_scheme ? ` · ${h.signature_scheme}` : ""}
                      </td>
                      <td>{h.enabled ? <Badge tone="ok">Enabled</Badge> : <Badge>Disabled</Badge>}</td>
                      <td>
                        <code className="small">{h.ingress_url}</code>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )
        }
      </Query>
      {editable && (
        <Dialog open={open} onOpenChange={setOpen} title="Add webhook" className="dialog-wide">
          <ErrorAlert error={error} />
          <form className="stack stack-sm" onSubmit={submit}>
            <div className="grid-2">
              <Field label="Name" htmlFor="wh-name">
                <input id="wh-name" required value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="internal-ci" />
              </Field>
              <Field label="Authentication" htmlFor="wh-auth">
                <select id="wh-auth" value={form.auth_mode} onChange={(e) => setForm({ ...form, auth_mode: e.target.value })}>
                  <option value="token">Bearer token</option>
                  <option value="url_token">URL token</option>
                  <option value="signature">Provider signature</option>
                </select>
              </Field>
              {form.auth_mode === "signature" && (
                <>
                  <Field label="Provider scheme" htmlFor="wh-scheme">
                    <select id="wh-scheme" value={form.signature_scheme} onChange={(e) => setForm({ ...form, signature_scheme: e.target.value })}>
                      <option value="">Choose…</option>
                      {SCHEMES.map((s) => (
                        <option key={s} value={s}>
                          {s}
                        </option>
                      ))}
                    </select>
                  </Field>
                  <Field label="Provider secret" htmlFor="wh-psecret" hint="Leave blank for a Standard Webhooks sender to have one generated, or if the provider issues its secret only after it has a URL.">
                    <input id="wh-psecret" type="password" autoComplete="off" value={form.secret} onChange={(e) => setForm({ ...form, secret: e.target.value })} />
                  </Field>
                </>
              )}
              <Field label="Secret name" htmlFor="wh-secret" hint="A managed team secret that stores the credential. Enter an existing secret's name to reuse it.">
                <input id="wh-secret" value={form.secret_name} onChange={(e) => setForm({ ...form, secret_name: e.target.value })} placeholder="Suggested from the name" />
              </Field>
              <Field label="Delivery ID header (optional)" htmlFor="wh-hdr">
                <input id="wh-hdr" value={form.delivery_id_header} onChange={(e) => setForm({ ...form, delivery_id_header: e.target.value })} placeholder="X-CI-Run-Id" />
              </Field>
            </div>
            <p className="muted small">
              The dashboard writes <code>webhooks/&lt;name&gt;.yaml</code> into the definition.
            </p>
            <div className="dialog-footer">
              <Button onClick={() => setOpen(false)}>Cancel</Button>
              <Button type="submit" variant="primary" busy={create.isPending}>
                Create webhook
              </Button>
            </div>
          </form>
        </Dialog>
      )}
    </>
  );
}

/** The headline of a filter test. A match does not start work while the automation is disabled. */
export function filterHeadline(r: Pick<TestFilterResult, "would_trigger" | "result">, automationEnabled?: boolean): { text: string; ok: boolean } {
  if (r.would_trigger && automationEnabled === false) return { text: "Would match, but the automation is disabled", ok: false };
  return { text: r.result ?? (r.would_trigger ? "Would trigger" : "Would not trigger"), ok: !!r.would_trigger };
}

function FilterResult({ r, automationEnabled }: { r: TestFilterResult; automationEnabled?: boolean }) {
  const head = filterHeadline(r, automationEnabled);
  return (
    <div className={`alert ${head.ok ? "alert-success" : "alert-warning"}`} role="status">
      <strong>{head.text}</strong>
      <ul className="small">
        <li>
          {r.webhook_check?.passed ? "✓" : "✗"} Webhook: {r.webhook_check?.detail}
        </li>
        <li>
          {r.payload_check?.passed ? "✓" : "✗"} Payload: {r.payload_check?.detail}
        </li>
      </ul>
      {r.delivery_id && <p className="small muted">Tested against delivery {r.delivery_id}.</p>}
    </div>
  );
}

/**
 * Tests a trigger filter (or an automation's webhook triggers) against a stored
 * delivery of any of the factory's webhooks.
 */
export function TestFilterPanel({
  factoryID,
  automation,
  webhookID,
  automationEnabled,
}: {
  factoryID: string;
  automation?: string;
  webhookID?: string;
  /** False when the tested automation is disabled. */
  automationEnabled?: boolean;
}) {
  const hooks = useWebhooks(factoryID);
  const [hookID, setHookID] = useState(webhookID ?? "");
  const activeHook = hookID || webhookID || hooks.data?.[0]?.id || "";
  const deliveries = useQuery({
    queryKey: ["deliveries", activeHook],
    enabled: !!activeHook,
    queryFn: () => unwrap(api.GET("/webhooks/{id}/deliveries", { params: { path: { id: activeHook }, query: { limit: 20 } } })).then((r) => ((r.items ?? []) as Delivery[]).filter((d) => d.status === "accepted")),
  });
  const [deliveryID, setDeliveryID] = useState("");
  const [filter, setFilter] = useState("");
  const [result, setResult] = useState<TestFilterResult | null>(null);
  const [error, setError] = useState<unknown>(null);
  const run = useMutate(
    (body: Schemas["FactoryTestFilterRequest"]) => unwrap(api.POST("/factories/{uid}/test-filter", { params: { path: { uid: factoryID } }, body })),
    { quietErrors: true, onSuccess: (r) => setResult(r) },
  );
  const did = deliveryID || deliveries.data?.[0]?.id || "";

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setResult(null);
    try {
      await run.mutateAsync(filter.trim() ? { delivery_id: did, filter: filter.trim() } : { delivery_id: did, automation });
    } catch (err) {
      setError(err);
    }
  }

  if (!hooks.data || hooks.data.length === 0) {
    return (
      <Card title="Test filter">
        <p className="muted small">Create a webhook and send it a delivery to test filters against real payloads.</p>
      </Card>
    );
  }
  return (
    <Card title="Test filter">
      <form className="stack" onSubmit={submit}>
        {!webhookID && (
          <Field label="Webhook" htmlFor="tf-hook">
            <select
              id="tf-hook"
              value={activeHook}
              onChange={(e) => {
                setHookID(e.target.value);
                setDeliveryID("");
              }}
            >
              {hooks.data.map((h) => (
                <option key={h.id} value={h.id}>
                  {h.name}
                </option>
              ))}
            </select>
          </Field>
        )}
        <Field label="Delivery" htmlFor="tf-delivery">
          <select id="tf-delivery" value={did} onChange={(e) => setDeliveryID(e.target.value)}>
            {(deliveries.data ?? []).length === 0 && <option value="">No accepted deliveries yet</option>}
            {(deliveries.data ?? []).map((d) => (
              <option key={d.id} value={d.id}>
                {d.label || d.delivery_id || d.id} · {fmtTime(d.received_at)}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Filter (JSON or YAML)" htmlFor="tf-filter" hint={automation !== undefined ? "Leave empty to try this automation's webhook triggers in order." : "For example: payload: { action: [deploy] }"}>
          <textarea id="tf-filter" rows={5} className="mono" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder={"webhook_ids: [my-hook]\npayload:\n  env: [prod]"} />
        </Field>
        <ErrorAlert error={error} />
        {result && <FilterResult r={result} automationEnabled={automationEnabled} />}
        <div className="form-actions">
          <Button type="submit" busy={run.isPending} disabled={!did}>
            Test
          </Button>
        </div>
      </form>
    </Card>
  );
}

export function WebhookPage() {
  const f = useFactory();
  const { id = "" } = useParams();
  const { canEditFactory } = useAuth();
  const navigate = useNavigate();
  const editable = canEditFactory(f.id) && f.source !== "git";
  const q = useQuery({ queryKey: ["webhook", id], queryFn: () => unwrap(api.GET("/webhooks/{id}", { params: { path: { id } } })) as Promise<Webhook> });
  const deliveries = useQuery({
    queryKey: ["deliveries", id, "all"],
    queryFn: () => unwrap(api.GET("/webhooks/{id}/deliveries", { params: { path: { id }, query: { limit: 50 } } })).then((r) => (r.items ?? []) as Delivery[]),
  });
  const [openDelivery, setOpenDelivery] = useState<string | null>(null);
  const delivery = useQuery({
    queryKey: ["delivery", id, openDelivery],
    enabled: !!openDelivery,
    queryFn: () => unwrap(api.GET("/webhooks/{id}/deliveries/{deliveryId}", { params: { path: { id, deliveryId: openDelivery! } } })) as Promise<Delivery>,
  });
  const [rotated, setRotated] = useState<string | null>(null);
  const toggle = useMutate((enabled: boolean) => unwrap(api.PATCH("/webhooks/{id}", { params: { path: { id } }, body: { enabled } })), {
    invalidate: [["webhook", id], ["webhooks", f.id]],
    success: (w) => `Webhook ${w.enabled ? "enabled" : "disabled"}.`,
  });
  const del = useMutate(() => unwrap(api.DELETE("/webhooks/{id}", { params: { path: { id } } })), {
    invalidate: [["webhooks", f.id], ["definition", f.id]],
    success: "Webhook deleted.",
    onSuccess: () => navigate(fpath(f, "webhooks")),
  });
  const rotate = useMutate(() => unwrap(api.POST("/webhooks/{id}/rotate-secret", { params: { path: { id } }, body: {} })), {
    success: "Secret rotated.",
    onSuccess: (r) => setRotated((r as { secret?: string }).secret ?? null),
  });

  return (
    <Query q={q}>
      {(h) => (
        <>
          <PageHeader
            title={
              <>
                {h.name} {h.enabled ? <Badge tone="ok">Enabled</Badge> : <Badge>Disabled</Badge>}
              </>
            }
            crumbs={[{ label: "Webhooks", to: fpath(f, "webhooks") }]}
            actions={
              editable && (
                <>
                  <Button onClick={() => toggle.mutate(!h.enabled)} busy={toggle.isPending}>
                    {h.enabled ? "Disable" : "Enable"}
                  </Button>
                  {h.auth_mode !== "signature" || h.signature_scheme === "standard_webhooks" ? (
                    <Button onClick={() => void confirm({ title: "Rotate the secret?", body: "Senders that use the old secret stop working.", confirmLabel: "Rotate secret", danger: true }).then((ok) => ok && rotate.mutate(undefined))} busy={rotate.isPending}>
                      Rotate secret
                    </Button>
                  ) : null}
                  <Button variant="danger" onClick={() => void confirm({ title: `Delete webhook ${h.name}?`, body: "Its URL stops accepting deliveries. You cannot undo this.", confirmLabel: "Delete webhook", danger: true }).then((ok) => ok && del.mutate(undefined))} busy={del.isPending}>
                    Delete
                  </Button>
                </>
              )
            }
          />
          <ReadOnlyNotice f={f} />
          {rotated && (
            <section className="card created" role="status">
              <CopyField id="r-secret" label="New secret (shown once)" value={rotated} />
            </section>
          )}
          <div className="two-col">
            <div className="stack">
              <Card title="Recent deliveries">
                {(deliveries.data ?? []).length === 0 ? (
                  <p className="muted small">No deliveries yet. POST JSON to the ingress URL to send one.</p>
                ) : (
                  <ul className="delivery-list">
                    {deliveries.data!.map((d) => (
                      <li key={d.id}>
                        <button type="button" className="btn-link-plain" aria-expanded={openDelivery === d.id} onClick={() => setOpenDelivery(openDelivery === d.id ? null : d.id)}>
                          <Badge tone={TONES_BY_STATUS[d.status] ?? "neutral"}>{d.label || d.status}</Badge> {d.delivery_id || d.id}
                        </button>{" "}
                        <span className="muted small">
                          <TimeAgo t={d.received_at} />
                          {d.reason ? ` · ${d.reason}` : ""}
                          {(d.run_ids ?? []).length > 0 && ` · ${d.run_ids!.length} run(s)`}
                        </span>
                        {openDelivery === d.id && delivery.data && (
                          <div className="delivery-detail">
                            {(delivery.data.run_ids ?? []).map((r) => (
                              <Link key={r} to={`/runs/${encodeURIComponent(r)}`} className="chip">
                                Run {r}
                              </Link>
                            ))}
                            <Code>{delivery.data.body !== undefined ? JSON.stringify(delivery.data.body, null, 2) : "(no stored payload)"}</Code>
                          </div>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
              </Card>
              <TestFilterPanel factoryID={f.id} webhookID={h.id} />
            </div>
            <aside className="stack">
              <Card title="Details">
                <CopyField id="w-url" label="Ingress URL" value={h.ingress_url ?? ""} />
                <KV
                  items={[
                    ["UID", <code key="id">{h.id}</code>],
                    ["Authentication", `${h.auth_mode}${h.signature_scheme ? ` · ${h.signature_scheme}` : ""}`],
                    ["Secret", <code key="s">{h.secret_name}</code>],
                    ["Delivery ID header", h.delivery_id_header || "—"],
                    ["Created", fmtTime(h.created_at)],
                  ]}
                />
                <WebhookUsage h={h} />
              </Card>
            </aside>
          </div>
        </>
      )}
    </Query>
  );
}
