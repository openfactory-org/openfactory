import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Server } from "lucide-react";
import { api, errorMessage, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { McpSignIns } from "../components/McpSignIns";
import { confirm } from "../components/overlays";
import { Alert, Badge, Button, Card, Empty, Field, PageHeader, Query, TimeAgo } from "../components/ui";
import { kvLines, lines, parseKVLines } from "../lib/format";
import { useMutate } from "../lib/mutate";

type MCPServer = Schemas["RegisteredMCPServer"];

function modeLabel(m: MCPServer): { label: string; tone: string } {
  if (m.oauth) {
    switch (m.oauth.status) {
      case "connected":
        return { label: "OAuth: connected", tone: "ok" };
      case "error":
        return { label: "OAuth: error", tone: "bad" };
      default:
        return { label: "OAuth: not connected", tone: "warn" };
    }
  }
  if (m.command) return { label: "Command", tone: "neutral" };
  return proxied(m) ? { label: "Proxied", tone: "info" } : { label: "Direct", tone: "warn" };
}

/** proxied reads the connection mode of a url server. */
function proxied(m: MCPServer): boolean {
  return m.connection === "proxy";
}

/** isSSE matches a legacy SSE URL, which the proxy cannot serve. */
function isSSE(url: string): boolean {
  try {
    return new URL(url.trim()).pathname.replace(/\/+$/, "").endsWith("/sse");
  } catch {
    return false;
  }
}

function transportOf(m: MCPServer): string {
  return m.url || [m.command, ...(m.args ?? [])].filter(Boolean).join(" ");
}

export function McpServersPage() {
  const { canEdit } = useAuth();
  const q = useQuery({ queryKey: ["mcp-servers"], queryFn: () => unwrap(api.GET("/mcp-servers")) });
  return (
    <>
      <PageHeader
        title="MCP servers"
        subtitle="Register MCP servers once for the team. Agents use them with mcpServers.<name>.managedId."
        actions={
          canEdit && (
            <Link className="btn btn-primary" to="/mcp-servers/new">
              Register server
            </Link>
          )
        }
      />
      <Query q={q}>
        {(data) => (
          <div className="stack">
            {(data.items ?? []).length === 0 ? (
              <Empty
                icon={<Server size={18} />}
                title="No registered MCP servers"
                action={
                  canEdit && (
                    <Link className="btn btn-primary" to="/mcp-servers/new">
                      Register server
                    </Link>
                  )
                }
              >
                Agents can still declare servers inline with <code>url</code> or <code>command</code>.
              </Empty>
            ) : (
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">Name</th>
                      <th scope="col">Transport</th>
                      <th scope="col">Mode</th>
                      <th scope="col">Updated</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(data.items ?? []).map((m) => {
                      const mode = modeLabel(m);
                      return (
                        <tr key={m.id}>
                          <td>
                            <Link to={`/mcp-servers/${encodeURIComponent(m.id)}`}>{m.name}</Link>
                            {m.description && <div className="muted small">{m.description}</div>}
                          </td>
                          <td>
                            <code className="small">{transportOf(m)}</code>
                          </td>
                          <td>
                            <Badge tone={mode.tone}>{mode.label}</Badge>
                          </td>
                          <td>
                            <TimeAgo t={m.updated_at} />
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
            <Card title="Built-in integration servers">
              <p className="muted small">
                Tools for the factory's own integrations, served by Open Factory with the integration's credentials. A factory must attach the integration. Write tools (comments,
                messages) need <code>allowWrite: true</code>.
              </p>
              <div className="table-wrap">
                <table className="table">
                  <thead>
                    <tr>
                      <th scope="col">Managed ID</th>
                      <th scope="col">Integration</th>
                    </tr>
                  </thead>
                  <tbody>
                    {(data.builtin ?? []).map((b) => (
                      <tr key={b.managed_id}>
                        <td>
                          <code>{b.managed_id}</code>
                        </td>
                        <td>{b.configured ? <Badge tone="ok">Configured</Badge> : <Link to="/integrations">Not configured</Link>}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </Card>
          </div>
        )}
      </Query>
      <McpSignIns />
    </>
  );
}

const emptyForm = {
  name: "",
  description: "",
  transport: "url" as "url" | "command",
  url: "",
  headers: "",
  command: "",
  args: "",
  env: "",
  // A url server uses the control-plane proxy by default.
  proxy: true,
  oauth: false,
  scopes: "",
  client_id: "",
  client_secret_name: "",
  authorization_endpoint: "",
  token_endpoint: "",
};

export function McpServerPage() {
  const { id = "" } = useParams();
  const isNew = id === "new";
  const navigate = useNavigate();
  const { canEdit } = useAuth();
  const [params, setParams] = useSearchParams();
  const q = useQuery({ queryKey: ["mcp-server", id], enabled: !isNew, queryFn: () => unwrap(api.GET("/mcp-servers/{id}", { params: { path: { id } } })) });
  const [form, setForm] = useState(emptyForm);
  const [error, setError] = useState<string | null>(null);
  const set = (patch: Partial<typeof emptyForm>) => setForm((f) => ({ ...f, ...patch }));

  useEffect(() => {
    const m = q.data;
    if (!m) return;
    setForm({
      name: m.name,
      description: m.description ?? "",
      transport: m.command ? "command" : "url",
      url: m.url ?? "",
      headers: kvLines(m.headers),
      command: m.command ?? "",
      args: (m.args ?? []).join("\n"),
      env: kvLines(m.env),
      proxy: m.command ? true : proxied(m),
      oauth: !!m.oauth,
      scopes: (m.oauth?.scopes ?? []).join(" "),
      client_id: m.oauth?.client_id ?? "",
      client_secret_name: m.oauth?.client_secret_name ?? "",
      authorization_endpoint: m.oauth?.authorization_endpoint ?? "",
      token_endpoint: m.oauth?.token_endpoint ?? "",
    });
  }, [q.data]);

  const save = useMutate(
    (body: Schemas["MCPServerRequest"]) =>
      isNew ? unwrap(api.POST("/mcp-servers", { body })) : unwrap(api.PATCH("/mcp-servers/{id}", { params: { path: { id } }, body })),
    {
      invalidate: [["mcp-servers"], ["mcp-server", id]],
      quietErrors: true,
      success: (m) => `MCP server ${m.name} saved.`,
      onSuccess: (m) => isNew && navigate(`/mcp-servers/${encodeURIComponent(m.id)}`, { replace: true }),
    },
  );
  const del = useMutate(() => unwrap(api.DELETE("/mcp-servers/{id}", { params: { path: { id } } })), {
    invalidate: [["mcp-servers"]],
    success: "MCP server deleted.",
    quietErrors: true,
    onSuccess: () => navigate("/mcp-servers"),
  });
  // The server refuses (409) to delete a server that an agent uses; show why.
  async function removeIt() {
    setError(null);
    try {
      await del.mutateAsync(undefined);
    } catch (err) {
      setError(errorMessage(err));
    }
  }
  const connect = useMutate(() => unwrap(api.POST("/mcp-servers/{id}/oauth/connect", { params: { path: { id } } })), {
    onSuccess: (r) => window.location.assign(r.authorization_url),
  });
  const disconnect = useMutate(() => unwrap(api.POST("/mcp-servers/{id}/oauth/disconnect", { params: { path: { id } } })), {
    invalidate: [["mcp-servers"], ["mcp-server", id]],
    success: "Disconnected.",
  });

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(form.name.trim())) return setError("Names use 1-64 letters, digits, '.', '_' or '-'.");
    let headers: Record<string, string> = {};
    let env: Record<string, string> = {};
    try {
      headers = form.transport === "url" ? parseKVLines(form.headers) : {};
      env = form.transport === "command" ? parseKVLines(form.env) : {};
    } catch (err) {
      return setError((err as Error).message);
    }
    const isURL = form.transport === "url";
    const body: Schemas["MCPServerRequest"] = {
      name: form.name.trim(),
      description: form.description.trim(),
      url: isURL ? form.url.trim() : "",
      headers,
      command: isURL ? "" : form.command.trim(),
      args: isURL ? [] : lines(form.args),
      env,
      // A legacy SSE server without OAuth cannot use the proxy: it is direct.
      connection: isURL ? (form.oauth || (form.proxy && !isSSE(form.url)) ? "proxy" : "direct") : undefined,
      oauth:
        isURL && form.oauth
          ? {
              scopes: form.scopes.split(/\s+/).filter(Boolean),
              client_id: form.client_id.trim(),
              client_secret_name: form.client_secret_name.trim(),
              authorization_endpoint: form.authorization_endpoint.trim(),
              token_endpoint: form.token_endpoint.trim(),
            }
          : null,
    };
    try {
      await save.mutateAsync(body);
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  const oauthResult = params.get("oauth");
  const m = q.data;
  const body = (
    <>
      <PageHeader
        title={isNew ? "Register MCP server" : form.name || id}
        crumbs={[{ label: "MCP servers", to: "/mcp-servers" }]}
        actions={
          canEdit &&
          !isNew && (
            <Button variant="danger" onClick={() => void confirm({ title: "Delete this MCP server?", body: "Its stored tokens are deleted too. You cannot delete it while an agent uses it.", confirmLabel: "Delete server", danger: true }).then((ok) => (ok ? removeIt() : undefined))} busy={del.isPending}>
              Delete
            </Button>
          )
        }
      />
      <div className="stack narrow">
        {oauthResult === "connected" && (
          <Alert kind="success">
            Connected. Runs can now use this server.{" "}
            <button type="button" className="btn-link" onClick={() => setParams({})}>
              Dismiss
            </button>
          </Alert>
        )}
        {oauthResult === "error" && <Alert kind="error">The connection failed: {params.get("message") || "unknown error"}</Alert>}
        {m?.oauth && (
          <Card title="OAuth connection">
            <p>
              <Badge tone={modeLabel(m).tone}>{modeLabel(m).label}</Badge>
              {m.oauth.connected_at && (
                <span className="muted small">
                  {" "}
                  by {m.oauth.connected_by} <TimeAgo t={m.oauth.connected_at} />
                </span>
              )}
            </p>
            {m.oauth.error && <Alert kind="error">{m.oauth.error}</Alert>}
            <p className="muted small">
              Tokens are stored encrypted on the server and added by the control-plane proxy. They never enter a sandbox. The provider redirects back to{" "}
              <code>/api/v1/mcp-servers/oauth/callback</code> on this server.
            </p>
            {canEdit && (
              <div className="form-actions">
                <Button variant="primary" onClick={() => connect.mutate(undefined)} busy={connect.isPending}>
                  {m.oauth.status === "connected" ? "Reconnect" : "Connect"}
                </Button>
                {m.oauth.status !== "not_connected" && (
                  <Button onClick={() => disconnect.mutate(undefined)} busy={disconnect.isPending}>
                    Disconnect
                  </Button>
                )}
              </div>
            )}
          </Card>
        )}
        {!isNew && m && (
          <Card title="Use it in an agent">
            <pre className="code small">{`mcpServers:\n  ${m.name}:\n    managedId: ${m.name}`}</pre>
          </Card>
        )}
        <form className="stack" onSubmit={submit}>
          {error && <Alert kind="error">{error}</Alert>}
          <fieldset disabled={!canEdit} className="plain-fieldset stack">
            <Field label="Name" htmlFor="m-name" hint="Agents reference the server by this name.">
              <input id="m-name" required value={form.name} onChange={(e) => set({ name: e.target.value })} />
            </Field>
            <Field label="Description" htmlFor="m-desc">
              <input id="m-desc" value={form.description} onChange={(e) => set({ description: e.target.value })} />
            </Field>
            <Field label="Transport" htmlFor="m-transport">
              <select id="m-transport" value={form.transport} onChange={(e) => set({ transport: e.target.value as "url" | "command" })}>
                <option value="url">Remote (streamable HTTP URL)</option>
                <option value="command">Local command in the sandbox (stdio)</option>
              </select>
            </Field>
            {form.transport === "url" ? (
              <>
                <Field label="URL" htmlFor="m-url" hint="Legacy SSE when the path ends in /sse. An SSE server cannot use the proxy or OAuth, so it connects from the sandbox.">
                  <input id="m-url" className="mono" required value={form.url} onChange={(e) => set({ url: e.target.value })} placeholder="https://mcp.example.com/mcp" />
                </Field>
                <Field label="Headers" htmlFor="m-headers" hint={<>One <code>NAME=VALUE</code> per line. Put credentials in secrets: <code>Authorization=Bearer {"{{MY_TOKEN}}"}</code>.</>}>
                  <textarea id="m-headers" rows={3} className="mono" value={form.headers} onChange={(e) => set({ headers: e.target.value })} />
                </Field>
                <label className="check">
                  <input
                    type="checkbox"
                    checked={form.oauth || (form.proxy && !isSSE(form.url))}
                    disabled={form.oauth || isSSE(form.url)}
                    onChange={(e) => set({ proxy: e.target.checked })}
                  />{" "}
                  Route through the control plane (default: headers never enter the sandbox). Clear it only for a server that the control plane cannot reach.
                </label>
                <label className="check">
                  <input type="checkbox" checked={form.oauth} onChange={(e) => set({ oauth: e.target.checked })} /> The server uses OAuth
                </label>
                {form.oauth && (
                  <>
                    <Field label="Scopes" htmlFor="m-scopes" hint="Space-separated. Empty requests the scopes the server advertises.">
                      <input id="m-scopes" className="mono" value={form.scopes} onChange={(e) => set({ scopes: e.target.value })} />
                    </Field>
                    <Field label="Client ID" htmlFor="m-client" hint="Leave empty to register a client automatically (dynamic client registration).">
                      <input id="m-client" className="mono" value={form.client_id} onChange={(e) => set({ client_id: e.target.value })} />
                    </Field>
                    <Field label="Client secret (secret name)" htmlFor="m-csecret" hint="Name of a team secret holding the client secret, for confidential clients.">
                      <input id="m-csecret" className="mono" value={form.client_secret_name} onChange={(e) => set({ client_secret_name: e.target.value })} />
                    </Field>
                    <details>
                      <summary>Endpoints (optional overrides)</summary>
                      <div className="stack">
                        <Field label="Authorization endpoint" htmlFor="m-authz">
                          <input id="m-authz" className="mono" value={form.authorization_endpoint} onChange={(e) => set({ authorization_endpoint: e.target.value })} />
                        </Field>
                        <Field label="Token endpoint" htmlFor="m-token">
                          <input id="m-token" className="mono" value={form.token_endpoint} onChange={(e) => set({ token_endpoint: e.target.value })} />
                        </Field>
                      </div>
                    </details>
                  </>
                )}
              </>
            ) : (
              <>
                <Field label="Command" htmlFor="m-command">
                  <input id="m-command" className="mono" required value={form.command} onChange={(e) => set({ command: e.target.value })} placeholder="npx" />
                </Field>
                <Field label="Arguments" htmlFor="m-args" hint="One per line.">
                  <textarea id="m-args" rows={3} className="mono" value={form.args} onChange={(e) => set({ args: e.target.value })} />
                </Field>
                <Field label="Environment variables" htmlFor="m-env" hint={<>One <code>KEY=VALUE</code> per line. Credentials as <code>{"{{SECRET_NAME}}"}</code>.</>}>
                  <textarea id="m-env" rows={3} className="mono" value={form.env} onChange={(e) => set({ env: e.target.value })} />
                </Field>
              </>
            )}
          </fieldset>
          {canEdit && (
            <div className="form-actions">
              <Button type="submit" variant="primary" busy={save.isPending}>
                {isNew ? "Register server" : "Save"}
              </Button>
            </div>
          )}
        </form>
      </div>
    </>
  );
  return isNew ? body : <Query q={q}>{() => body}</Query>;
}
