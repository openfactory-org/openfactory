import { useEffect, useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { factoryKey, fpath, useFactory } from "../components/FactoryLayout";
import { FACTORIES_KEY } from "../components/Layout";
import { Alert, Badge, Button, Card, ErrorAlert, Field, PageHeader, Query } from "../components/ui";
import { useMutate } from "../lib/mutate";
import { INTEGRATION_TYPES } from "./Factories";

type UpdateFactory = Schemas["UpdateFactoryRequest"];

/**
 * Checks a git repository of a definition source with the rule of the server
 * (parseGitRepo of the control plane): it must name owner/repository,
 * with or without a host. It returns an error text, or null when it is valid.
 */
export function gitRepoError(raw: string): string | null {
  const s = raw.trim();
  if (!s) return "Enter the repository that holds factory.yaml.";
  let path = s;
  try {
    const u = new URL(s);
    if (u.host) path = u.pathname;
  } catch {
    const i = s.indexOf("/");
    if (i > 0 && s.slice(0, i).includes(".")) path = s.slice(i + 1);
  }
  path = path.replace(/^\/+|\/+$/g, "").replace(/\.git$/, "");
  const i = path.lastIndexOf("/");
  if (i <= 0 || i === path.length - 1) return `The repository must name owner/repository, for example acme/factory-def (you entered "${s}").`;
  return null;
}

/** The identity fields that differ from the saved factory. */
export function changedIdentity(
  form: { name: string; alias: string; description: string; credential_strategy: string },
  saved: { name: string; alias: string; description: string; credential_strategy: string },
): UpdateFactory {
  const body: UpdateFactory = {};
  if (form.name.trim() !== saved.name) body.name = form.name.trim();
  if (form.alias.trim() !== saved.alias) body.alias = form.alias.trim();
  if (form.description.trim() !== saved.description) body.description = form.description.trim();
  if (form.credential_strategy !== saved.credential_strategy) body.credential_strategy = form.credential_strategy as UpdateFactory["credential_strategy"];
  return body;
}

export function SettingsPage() {
  const f = useFactory();
  const { canEditFactory, isFactoryAdmin } = useAuth();
  const canEdit = canEditFactory(f.id);
  const isAdmin = isFactoryAdmin(f.id);
  const navigate = useNavigate();
  const settings = useQuery({
    queryKey: ["settings", f.id, f.active_version_id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/settings", { params: { path: { uid: f.id } } })),
  });
  const managed = f.source !== "git";
  const [identity, setIdentity] = useState({ name: f.name, alias: f.alias, description: f.description ?? "", credential_strategy: "EXECUTOR" });
  const [source, setSource] = useState({ source: f.source, git_repo: f.git_repo ?? "", git_branch: f.git_branch ?? "main", git_path: f.git_path ?? "" });
  const [confirm, setConfirm] = useState("");
  const [idError, setIdError] = useState<unknown>(null);
  const [srcError, setSrcError] = useState<unknown>(null);
  const [idNote, setIdNote] = useState<string | null>(null);
  const [srcNote, setSrcNote] = useState<string | null>(null);
  const savedStrategy = settings.data?.credential_strategy || "EXECUTOR";
  useEffect(() => {
    if (settings.data) setIdentity((s) => ({ ...s, credential_strategy: settings.data.credential_strategy || "EXECUTOR" }));
  }, [settings.data]);

  const patchFactory = (body: UpdateFactory) => unwrap(api.PATCH("/factories/{uid}", { params: { path: { uid: f.id } }, body }));
  const invalidate = [factoryKey(f.id), FACTORIES_KEY, ["settings", f.id], ["definition", f.id]];
  const patchIdentity = useMutate(patchFactory, { invalidate, quietErrors: true, success: "Settings saved." });
  const patchSource = useMutate(patchFactory, { invalidate, quietErrors: true, success: "Definition source updated." });
  const del = useMutate(() => unwrap(api.DELETE("/factories/{uid}", { params: { path: { uid: f.id } } })), {
    invalidate: [FACTORIES_KEY],
    success: `Factory ${f.name} deleted.`,
    onSuccess: () => navigate("/factories"),
  });

  async function saveIdentity(e: FormEvent) {
    e.preventDefault();
    setIdError(null);
    setIdNote(null);
    if (!identity.name.trim()) return setIdError("Enter a factory name.");
    if (identity.alias && !/^[A-Za-z0-9 ._-]{1,60}$/.test(identity.alias)) return setIdError("Foreman name: use up to 60 letters, numbers, spaces, '.', '_' and '-'.");
    // Send only the changed fields: each save writes a new definition version.
    const body = changedIdentity(identity, { name: f.name, alias: f.alias, description: f.description ?? "", credential_strategy: savedStrategy });
    if (Object.keys(body).length === 0) return setIdNote("Nothing changed, so nothing was saved.");
    try {
      await patchIdentity.mutateAsync(body);
      setIdNote("Saved.");
    } catch (err) {
      setIdError(err);
    }
  }

  async function saveSource(e: FormEvent) {
    e.preventDefault();
    setSrcError(null);
    setSrcNote(null);
    if (source.source === "git") {
      const bad = gitRepoError(source.git_repo);
      if (bad) return setSrcError(bad);
    }
    const unchanged =
      source.source === f.source &&
      (source.source !== "git" ||
        (source.git_repo.trim() === (f.git_repo ?? "") && (source.git_branch.trim() || "main") === (f.git_branch || "main") && source.git_path.trim().replace(/^\/+|\/+$/g, "") === (f.git_path ?? "")));
    if (unchanged) return setSrcNote("Nothing changed, so nothing was saved.");
    try {
      await patchSource.mutateAsync(
        source.source === "git"
          ? { source: "git", git_repo: source.git_repo.trim(), git_branch: source.git_branch.trim() || "main", git_path: source.git_path.trim() }
          : { source: "managed" },
      );
      setSrcNote("Saved.");
    } catch (err) {
      setSrcError(err);
    }
  }

  return (
    <>
      <PageHeader title="Settings" crumbs={[{ label: f.name, to: `/f/${encodeURIComponent(f.id)}` }]} />
      <div className="stack narrow">
        <Card title="Identity">
          {!managed && (
            <Alert kind="info">
              This factory's definition lives in <code>{f.git_repo}</code>; change its identity in <code>factory.yaml</code> there.
            </Alert>
          )}
          <form className="stack" onSubmit={saveIdentity}>
            <ErrorAlert error={idError} />
            {idNote && <Alert kind={idNote === "Saved." ? "success" : "info"}>{idNote}</Alert>}
            <fieldset disabled={!canEdit || !managed} className="plain-fieldset stack">
              <Field label="Name" htmlFor="st-name">
                <input id="st-name" required value={identity.name} onChange={(e) => setIdentity({ ...identity, name: e.target.value })} />
              </Field>
              <Field label="Foreman name" htmlFor="st-alias" hint="The handle your team @-mentions.">
                <input id="st-alias" value={identity.alias} onChange={(e) => setIdentity({ ...identity, alias: e.target.value })} />
              </Field>
              <Field label="Description" htmlFor="st-desc">
                <textarea id="st-desc" rows={2} value={identity.description} onChange={(e) => setIdentity({ ...identity, description: e.target.value })} />
              </Field>
              <Field label="Pull request authorship" htmlFor="st-cred" hint="Whether pull requests are authored by the agent (EXECUTOR) or the person who started the run (CREATOR).">
                <select id="st-cred" value={identity.credential_strategy} onChange={(e) => setIdentity({ ...identity, credential_strategy: e.target.value })}>
                  <option value="EXECUTOR">Agent (EXECUTOR)</option>
                  <option value="CREATOR">Run creator (CREATOR)</option>
                </select>
              </Field>
            </fieldset>
            {canEdit && managed && (
              <div className="form-actions">
                <Button type="submit" variant="primary" busy={patchIdentity.isPending}>
                  Save
                </Button>
              </div>
            )}
          </form>
        </Card>
        <Query q={settings}>
          {(s) => (
            <>
              <Card title="Repositories">
                {s.repositories.length === 0 ? (
                  <p className="muted small">No repositories declared.</p>
                ) : (
                  <ul>
                    {s.repositories.map((r) => (
                      <li key={`${r.owner}/${r.name}`}>
                        <code>
                          {r.owner}/{r.name}
                        </code>{" "}
                        <span className="muted small">{r.codeForge ?? "GITHUB"}</span>
                      </li>
                    ))}
                  </ul>
                )}
                {managed && (
                  <p className="small">
                    Edit <code>repositories</code> in <Link to={`${fpath(f, "definition")}?file=factory.yaml`}>factory.yaml</Link>.
                  </p>
                )}
              </Card>
              <Card title="Machines">
                {s.machines.length === 0 ? (
                  <p className="muted small">
                    No machines declared; runs use the default sandbox. Add <code>machines/&lt;name&gt;.yaml</code> to the definition.
                  </p>
                ) : (
                  <table className="table">
                    <thead>
                      <tr>
                        <th scope="col">Machine</th>
                        <th scope="col">Platform</th>
                        <th scope="col">Size</th>
                        <th scope="col">Image</th>
                      </tr>
                    </thead>
                    <tbody>
                      {s.machines.map((r) => (
                        <tr key={r.name}>
                          <td>{r.name}</td>
                          <td>
                            {r.os}/{r.arch}
                          </td>
                          <td>{r.vcpus ? `${r.vcpus} vCPU · ${r.memoryGb ?? "?"} GB` : "—"}</td>
                          <td>
                            <code className="small">{r.image || "default"}</code>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </Card>
            </>
          )}
        </Query>
        <Card title="Definition source">
          <form className="stack" onSubmit={saveSource}>
            <ErrorAlert error={srcError} />
            {srcNote && <Alert kind={srcNote === "Saved." ? "success" : "info"}>{srcNote}</Alert>}
            <fieldset disabled={!canEdit} className="plain-fieldset stack">
              <fieldset className="field">
                <legend>Where the definition lives</legend>
                <div className="check">
                  <input id="st-src-managed" type="radio" name="source" value="managed" checked={source.source !== "git"} onChange={() => setSource({ ...source, source: "managed" })} />
                  <label htmlFor="st-src-managed">Managed here (edit in the dashboard and API)</label>
                </div>
                <div className="check">
                  <input id="st-src-git" type="radio" name="source" value="git" checked={source.source === "git"} onChange={() => setSource({ ...source, source: "git" })} />
                  <label htmlFor="st-src-git">A git repository (changes through pull requests)</label>
                </div>
              </fieldset>
              {source.source === "git" && (
                <div className="grid-2">
                  <Field label="Repository" htmlFor="st-repo" hint="owner/repository, with or without a host, for example acme/factory-def.">
                    <input id="st-repo" className="mono" value={source.git_repo} onChange={(e) => setSource({ ...source, git_repo: e.target.value })} placeholder="acme/factory-def" />
                  </Field>
                  <Field label="Branch" htmlFor="st-branch">
                    <input id="st-branch" value={source.git_branch} onChange={(e) => setSource({ ...source, git_branch: e.target.value })} />
                  </Field>
                  <Field label="Path" htmlFor="st-path" hint="Directory that holds factory.yaml (empty for the root).">
                    <input id="st-path" className="mono" value={source.git_path} onChange={(e) => setSource({ ...source, git_path: e.target.value })} />
                  </Field>
                </div>
              )}
            </fieldset>
            {canEdit && (
              <div className="form-actions">
                <Button type="submit" busy={patchSource.isPending}>
                  Update source
                </Button>
              </div>
            )}
          </form>
        </Card>
        {isAdmin && (
          <Card title="Delete factory" className="danger-zone">
            <p className="small">Deletes the factory, its definition history, work items and runs. This cannot be undone.</p>
            <form
              className="input-row"
              onSubmit={(e) => {
                e.preventDefault();
                if (confirm === f.name) del.mutate(undefined);
              }}
            >
              <label className="sr-only" htmlFor="st-confirm">
                Type the factory name to confirm
              </label>
              <input id="st-confirm" value={confirm} onChange={(e) => setConfirm(e.target.value)} placeholder={`Type "${f.name}" to confirm`} />
              <Button type="submit" variant="danger" disabled={confirm !== f.name} busy={del.isPending}>
                Delete factory
              </Button>
            </form>
          </Card>
        )}
      </div>
    </>
  );
}

export function FactoryIntegrationsPage() {
  const f = useFactory();
  const { canEditFactory } = useAuth();
  const attached = useQuery({
    queryKey: ["factory-integrations", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/integrations", { params: { path: { uid: f.id } } })).then((r) => r.integrations ?? {}),
  });
  const team = useQuery({ queryKey: ["integrations"], queryFn: () => unwrap(api.GET("/integrations")).then((r) => r.items ?? []) });
  const [selected, setSelected] = useState<string[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const current = selected ?? Object.keys(attached.data ?? {});
  const editable = canEditFactory(f.id) && f.source !== "git";
  const save = useMutate(
    (integrations: string[]) =>
      unwrap(api.PUT("/factories/{uid}/integrations", { params: { path: { uid: f.id } }, body: { integrations: integrations as Schemas["PutFactoryIntegrationsRequest"]["integrations"] } })),
    { invalidate: [["factory-integrations", f.id], ["definition", f.id]], quietErrors: true, success: "Integrations updated.", onSuccess: () => setSelected(null) },
  );

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    if (current.includes("linear") && current.includes("jira")) return setError("Declare at most one issue tracker: Linear and Jira are mutually exclusive.");
    try {
      await save.mutateAsync(current);
    } catch (err) {
      setError(err);
    }
  }

  const connected = (type: string) => (team.data ?? []).filter((i) => i.provider === type.replace(/-/g, "_"));

  return (
    <>
      <PageHeader title="Integrations" crumbs={[{ label: f.name, to: `/f/${encodeURIComponent(f.id)}` }]} subtitle="Chat and tracker providers this factory listens to and replies on." />
      {f.source === "git" && (
        <Alert kind="info">
          Integrations of a git-backed factory are declared in its <code>factory.yaml</code>.
        </Alert>
      )}
      <Query q={attached}>
        {() => (
          <form className="stack narrow" onSubmit={submit}>
            <ErrorAlert error={error} />
            <fieldset disabled={!editable} className="plain-fieldset">
              <legend className="sr-only">Integrations</legend>
              <ul className="option-list">
                {INTEGRATION_TYPES.map((it) => {
                  const conns = connected(it.type);
                  return (
                    <li key={it.type} className="option-row">
                      <label className="check" htmlFor={`fi-${it.type}`}>
                        <input
                          id={`fi-${it.type}`}
                          type="checkbox"
                          value={it.type}
                          aria-labelledby={`fi-${it.type}-l`}
                          aria-describedby={`fi-${it.type}-d`}
                          checked={current.includes(it.type)}
                          onChange={(e) => setSelected(e.target.checked ? [...current, it.type] : current.filter((x) => x !== it.type))}
                        />
                        <span>
                          <strong id={`fi-${it.type}-l`}>{it.label}</strong>
                          <span id={`fi-${it.type}-d`} className="muted small">
                            {it.hint}
                          </span>
                        </span>
                      </label>
                      <span className="option-status">
                        {conns.length > 0 ? (
                          <Badge tone="ok">Connected: {conns.map((c) => c.name || c.provider).join(", ")}</Badge>
                        ) : (
                          <>
                            <span className="muted">Not connected</span>
                            <Link to={`/integrations?provider=${encodeURIComponent(it.type)}`}>Connect {it.label}</Link>
                          </>
                        )}
                      </span>
                    </li>
                  );
                })}
              </ul>
            </fieldset>
            {editable && (
              <div className="form-actions">
                <Button type="submit" variant="primary" busy={save.isPending}>
                  Save
                </Button>
              </div>
            )}
          </form>
        )}
      </Query>
    </>
  );
}
