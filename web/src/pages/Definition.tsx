import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { History } from "lucide-react";
import { api, ApiError, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { fpath, useFactory } from "../components/FactoryLayout";
import { confirm, Dialog } from "../components/overlays";
import { Alert, Badge, Button, Empty, Field, Keys, PageHeader, Query, TimeAgo, ValidationList } from "../components/ui";
import { MOD } from "../lib/queries";
import { diffFiles, hunks, type FileDiff } from "../lib/diff";
import { shortID } from "../lib/format";
import { useMutate } from "../lib/mutate";

type Version = Schemas["DefinitionVersion"];
type VErr = Schemas["ValidationError"];

export function definitionKey(fid: string) {
  return ["definition", fid] as const;
}

/** Validates a definition path the way the server does (relative, no "..", no leading slash). */
export function cleanDefPath(p: string): string {
  const v = p.trim().replace(/\\/g, "/");
  if (!v) throw new Error("Enter a file path.");
  if (v.startsWith("/") || v.split("/").some((seg) => seg === ".." || seg === "." || seg === "")) throw new Error("Use a relative path without '.', '..' or empty segments.");
  if (!/^[A-Za-z0-9._\-/]+$/.test(v)) throw new Error("Paths may contain letters, numbers, '.', '_', '-' and '/'.");
  return v;
}

/**
 * The change message of a definition save when the user leaves it empty:
 * "Add <path>" for a new file, "Delete <path>", "Rename <old> to <new>" or
 * "Update <path>".
 */
export function defaultChangeMessage({ isNew, del, selected, path }: { isNew: boolean; del: boolean; selected: string; path: string }): string {
  if (del) return `Delete ${selected || path}`;
  if (isNew) return `Add ${path}`;
  if (selected && path !== selected) return `Rename ${selected} to ${path}`;
  return `Update ${path}`;
}

/**
 * A plain-text editor with line numbers. It grows with its content, marks the
 * lines that have validation errors, and saves on ⌘S.
 */
function CodeEditor({
  id,
  value,
  onChange,
  readOnly,
  errorLines,
  onSave,
}: {
  id: string;
  value: string;
  onChange: (v: string) => void;
  readOnly?: boolean;
  errorLines: number[];
  onSave: () => void;
}) {
  const count = Math.max(value.split("\n").length, 12);
  return (
    <div className="code-editor">
      <div className="code-gutter" aria-hidden="true">
        {Array.from({ length: count }, (_, i) => (
          <div key={i} className={errorLines.includes(i + 1) ? "err" : undefined}>
            {i + 1}
          </div>
        ))}
      </div>
      <textarea
        id={id}
        rows={count}
        wrap="off"
        spellCheck={false}
        readOnly={readOnly}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
            e.preventDefault();
            onSave();
          }
        }}
      />
    </div>
  );
}

function FileTree({ paths, selected, onSelect, errorCounts }: { paths: string[]; selected: string; onSelect: (p: string) => void; errorCounts: Record<string, number> }) {
  return (
    <ul className="file-tree" aria-label="Definition files">
      {paths.map((p) => {
        const depth = p.split("/").length - 1;
        return (
          <li key={p} style={{ paddingLeft: `${depth * 0.75}rem` }}>
            <button type="button" className={`file-link ${p === selected ? "active" : ""}`} aria-current={p === selected ? "true" : undefined} onClick={() => onSelect(p)}>
              {p}
              {errorCounts[p] ? (
                <span className="badge badge-bad" aria-label={`${errorCounts[p]} errors`}>
                  {errorCounts[p]}
                </span>
              ) : null}
            </button>
          </li>
        );
      })}
    </ul>
  );
}

export function DefinitionPage() {
  const f = useFactory();
  const { canEditFactory } = useAuth();
  const canEdit = canEditFactory(f.id);
  const [params, setParams] = useSearchParams();
  const q = useQuery({ queryKey: definitionKey(f.id), queryFn: () => unwrap(api.GET("/factories/{uid}/definition", { params: { path: { uid: f.id } } })) });
  const latest = useQuery({
    queryKey: ["versions", f.id, "latest"],
    queryFn: () => unwrap(api.GET("/factories/{uid}/definition/versions", { params: { path: { uid: f.id }, query: { limit: 1 } } })).then((r) => r.items?.[0]),
  });
  const files = useMemo(() => (q.data?.files ?? {}) as Record<string, string>, [q.data]);
  const paths = useMemo(() => Object.keys(files).sort(), [files]);
  const isNew = params.get("new") === "1" || (q.isSuccess && paths.length === 0);
  const selected = isNew ? "" : params.get("file") && files[params.get("file")!] !== undefined ? params.get("file")! : paths.includes("factory.yaml") ? "factory.yaml" : paths[0] ?? "";
  const [path, setPath] = useState("");
  const [content, setContent] = useState("");
  const [message, setMessage] = useState("");
  const [errors, setErrors] = useState<VErr[]>([]);
  const [checked, setChecked] = useState<null | boolean>(null);
  const [review, setReview] = useState<FileDiff[] | null>(null);
  const editable = canEdit && f.source !== "git";

  useEffect(() => {
    if (!q.data) return;
    if (isNew) {
      setPath(paths.length === 0 ? "factory.yaml" : "");
      setContent(paths.length === 0 ? `schemaVersion: v1\nname: ${f.name}\nrepositories: []\nagentDefaults:\n  model: auto\n` : "");
    } else {
      setPath(selected);
      setContent(files[selected] ?? "");
    }
    setErrors([]);
    setChecked(null);
  }, [q.data, selected, isNew, files, paths.length, f.name]);

  const editedFiles = (del = false): Record<string, string> => {
    const next = { ...files };
    const clean = cleanDefPath(path);
    if (!isNew && selected && selected !== clean) delete next[selected];
    if (del) delete next[clean];
    else {
      if (isNew && files[clean] !== undefined) throw new Error("A file with this path already exists.");
      if (content.length > 512 * 1024) throw new Error("File is larger than 512 KB.");
      next[clean] = content.replace(/\r\n/g, "\n");
    }
    return next;
  };

  const validate = useMutate((fs: Record<string, string>) => unwrap(api.POST("/factories/{uid}/definition/validate", { params: { path: { uid: f.id } }, body: { files: fs } })), {
    onSuccess: (r) => {
      setErrors(r.errors ?? []);
      setChecked(r.valid);
    },
  });
  const save = useMutate(
    ({ fs, msg }: { fs: Record<string, string>; msg: string }) =>
      unwrap(api.PUT("/factories/{uid}/definition", { params: { path: { uid: f.id } }, body: { files: fs, message: msg, base_version_id: q.data?.version?.id } })),
    {
      quietErrors: true,
      invalidate: [definitionKey(f.id), ["versions", f.id], ["agents", f.id], ["automations", f.id], ["webhooks", f.id], ["factory", f.id]],
      success: (r) => `Definition saved and applied${r.version?.id ? ` (version ${shortID(r.version.id)})` : ""}.`,
    },
  );

  function local(fn: () => void) {
    setErrors([]);
    setChecked(null);
    try {
      fn();
    } catch (err) {
      setErrors([{ file: path || "(path)", message: (err as Error).message }]);
    }
  }

  async function doSave(del = false) {
    setErrors([]);
    setChecked(null);
    let fs: Record<string, string>;
    let clean: string;
    try {
      fs = editedFiles(del);
      clean = cleanDefPath(path);
    } catch (err) {
      setErrors([{ file: path || "(path)", message: (err as Error).message }]);
      return;
    }
    const msg = message.trim() || defaultChangeMessage({ isNew, del, selected, path: clean });
    try {
      await save.mutateAsync({ fs, msg });
      setMessage("");
      setParams(del ? {} : { file: clean }, { replace: true });
    } catch (err) {
      setErrors(err instanceof ApiError && err.validationErrors.length ? err.validationErrors : [{ file: clean, message: (err as Error).message }]);
    }
  }

  const errorCounts = errors.reduce<Record<string, number>>((m, e) => ({ ...m, [e.file]: (m[e.file] ?? 0) + 1 }), {});
  const dirty = editable && !!q.data && (isNew ? content !== "" && paths.length > 0 : content !== (files[selected] ?? "") || path !== selected);
  const newerBad = latest.data && !latest.data.valid && latest.data.id !== q.data?.version?.id ? latest.data : null;

  if (f.source === "git") {
    return (
      <>
        <PageHeader title="Factory definition" />
        <Alert kind="info">
          This factory's definition lives in <code>{f.git_repo}</code>. Edit it through pull requests; <Link to={fpath(f, "settings")}>Settings</Link> shows the source.
        </Alert>
      </>
    );
  }

  return (
    <Query q={q}>
      {(def) => (
        <>
          <PageHeader
            title="Factory definition"
            crumbs={[{ label: f.name, to: fpath(f) }]}
            subtitle={
              def.version ? (
                <>
                  Active version <code>{shortID(def.version.id)}</code> · {def.version.message || "no message"} · <TimeAgo t={def.version.created_at} />
                </>
              ) : (
                "No definition applied yet."
              )
            }
            actions={
              <>
                <Link className="btn btn-secondary" to={fpath(f, "definition/versions")}>
                  Version history
                </Link>
                {editable && (
                  <Link className="btn btn-secondary" to={`${fpath(f, "definition")}?new=1`}>
                    New file
                  </Link>
                )}
              </>
            }
          />
          {newerBad && (
            <Alert kind="warning">
              A newer version (<Link to={fpath(f, `definition/versions/${newerBad.id}`)}>{shortID(newerBad.id)}</Link>) failed validation and was not applied.
            </Alert>
          )}
          <div className="editor-layout">
            <nav className="editor-files" aria-label="Files">
              <FileTree paths={paths} selected={selected} errorCounts={errorCounts} onSelect={(p) => setParams({ file: p }, { replace: true })} />
            </nav>
            <form
              className="stack"
              onSubmit={(e) => {
                e.preventDefault();
                void doSave(false);
              }}
            >
              <ValidationList errors={errors} />
              {checked === true && <Alert kind="success">The definition is valid.</Alert>}
              <Field label="Path" htmlFor="path">
                <input id="path" className="mono" value={path} readOnly={!editable} onChange={(e) => setPath(e.target.value)} placeholder="agents/my-agent/agent.md" required />
              </Field>
              <div className="field">
                <label htmlFor="content" className="row">
                  Content
                  {dirty && (
                    <span className="row muted" style={{ fontWeight: 400 }}>
                      <span className="dirty-dot" aria-hidden="true" /> Unsaved changes
                    </span>
                  )}
                </label>
                <CodeEditor
                  id="content"
                  value={content}
                  readOnly={!editable}
                  onChange={setContent}
                  errorLines={errors.filter((e) => e.file === path && e.line).map((e) => e.line!)}
                  onSave={() => editable && void doSave(false)}
                />
              </div>
              {editable && (
                <>
                  <Field label="Change message" htmlFor="msg">
                    <input id="msg" value={message} onChange={(e) => setMessage(e.target.value)} placeholder={defaultChangeMessage({ isNew, del: false, selected, path: path.trim() || "file" })} />
                  </Field>
                  <div className="form-actions">
                    {dirty && <Button onClick={() => local(() => setReview(diffFiles(files, editedFiles(false)).filter((d) => d.added + d.removed > 0)))}>Review changes</Button>}
                    <Button onClick={() => local(() => validate.mutate(editedFiles(false)))} busy={validate.isPending}>
                      Validate
                    </Button>
                    {/* The server always refuses to delete factory.yaml. */}
                    {!isNew && selected !== "factory.yaml" && (
                      <Button variant="danger" onClick={() => void confirm({ title: `Delete ${selected}?`, body: "The file is removed and the definition is applied as a new version.", confirmLabel: "Delete file", danger: true }).then((ok) => ok && void doSave(true))} busy={save.isPending}>
                        Delete file
                      </Button>
                    )}
                    <Button type="submit" variant="primary" busy={save.isPending}>
                      Save and apply <Keys k={`${MOD} S`} />
                    </Button>
                  </div>
                  <p className="muted small">Saving validates the whole definition and applies every file together as a new version.</p>
                </>
              )}
            </form>
          </div>
          <Dialog
            open={review !== null}
            onOpenChange={(o) => !o && setReview(null)}
            title="Review changes"
            description="Saving applies these changes as a new version of the definition."
            className="dialog-wide"
            footer={
              <>
                <Button onClick={() => setReview(null)}>Keep editing</Button>
                <Button
                  variant="primary"
                  busy={save.isPending}
                  onClick={() => {
                    setReview(null);
                    void doSave(false);
                  }}
                >
                  Save and apply
                </Button>
              </>
            }
          >
            <div className="stack stack-sm">{(review ?? []).length === 0 ? <p className="muted">No changes.</p> : (review ?? []).map((d) => <FileDiffView key={d.path} d={d} />)}</div>
          </Dialog>
        </>
      )}
    </Query>
  );
}

/** Names the author of a version: a person, an agent run or the system. */
function authorLabel(author: string | undefined, myID: string | undefined): string {
  const a = author ?? "";
  if (a.startsWith("user:")) return a.slice(5) === myID ? "You" : "A team member";
  if (a.startsWith("run:")) return "An agent run";
  if (!a || a === "system") return "The factory";
  return a;
}

export function VersionsPage() {
  const f = useFactory();
  const { me } = useAuth();
  const q = useQuery({
    queryKey: ["versions", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/definition/versions", { params: { path: { uid: f.id }, query: { limit: 100 } } })),
  });
  return (
    <>
      <PageHeader title="Version history" crumbs={[{ label: "Factory definition", to: fpath(f, "definition") }]} />
      <Query q={q}>
        {(r) =>
          (r.items ?? []).length === 0 ? (
            <Empty icon={<History size={18} />} title="No versions yet" />
          ) : (
            <ul className="list bleed" aria-label="Versions">
              {r.items.map((v) => (
                <li key={v.id}>
                  <div className="list-row">
                    <History size={14} aria-hidden="true" className="faint" />
                    <Link to={fpath(f, `definition/versions/${encodeURIComponent(v.id)}`)} className="row-id" data-row>
                      <code>{shortID(v.id)}</code>
                    </Link>
                    <span className="row-title">
                      {v.message || "No message"} {v.id === (r.active_version_id ?? f.active_version_id) && <Badge tone="info">Active</Badge>}
                    </span>
                    <span className="row-meta">
                      <span className="hide-sm">{authorLabel(v.author, me?.user.id)}</span>
                      {v.valid ? <Badge tone="ok">Valid</Badge> : <Badge tone="bad">Invalid</Badge>}
                    </span>
                    <span className="row-time">
                      <TimeAgo t={v.created_at} />
                    </span>
                  </div>
                </li>
              ))}
            </ul>
          )
        }
      </Query>
    </>
  );
}

function FileDiffView({ d }: { d: FileDiff }) {
  return (
    <section className="card diff-card">
      <h2 className="diff-file">
        <code>{d.path}</code> <Badge tone={d.status === "added" ? "ok" : d.status === "removed" ? "bad" : "info"}>{d.status}</Badge>{" "}
        <span className="muted small">
          +{d.added} −{d.removed}
        </span>
      </h2>
      <table className="diff">
        <tbody>
          {hunks(d.ops).map((o, i) =>
            o.kind === "gap" ? (
              <tr key={i} className="diff-gap">
                <td colSpan={3}>⋯ {o.count} unchanged lines</td>
              </tr>
            ) : (
              <tr key={i} className={`diff-${o.kind}`}>
                <td className="ln">{o.oldNo ?? ""}</td>
                <td className="ln">{o.newNo ?? ""}</td>
                <td className="code-line">
                  <span aria-hidden="true">{o.kind === "add" ? "+" : o.kind === "del" ? "−" : " "}</span>
                  <span className="sr-only">{o.kind === "add" ? "added: " : o.kind === "del" ? "removed: " : ""}</span>
                  {o.text}
                </td>
              </tr>
            ),
          )}
        </tbody>
      </table>
    </section>
  );
}

export function VersionPage() {
  const f = useFactory();
  const { vid = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const { canEditFactory } = useAuth();
  const canEdit = canEditFactory(f.id);
  const versions = useQuery({
    queryKey: ["versions", f.id, "files"],
    queryFn: () => unwrap(api.GET("/factories/{uid}/definition/versions", { params: { path: { uid: f.id }, query: { limit: 200 } } })).then((r) => r.items ?? []),
  });
  const v = useQuery({
    queryKey: ["version", vid],
    queryFn: () => unwrap(api.GET("/factories/{uid}/definition/versions/{versionId}", { params: { path: { uid: f.id, versionId: vid } } })) as Promise<Version>,
  });
  const list = versions.data ?? [];
  const against = params.get("against") ?? list[list.findIndex((x) => x.id === vid) + 1]?.id ?? "";
  const base = useQuery({
    queryKey: ["version", against],
    enabled: !!against && list.findIndex((x) => x.id === vid) >= 0,
    queryFn: () => unwrap(api.GET("/factories/{uid}/definition/versions/{versionId}", { params: { path: { uid: f.id, versionId: against } } })) as Promise<Version>,
  });
  const restore = useMutate(() => unwrap(api.POST("/factories/{uid}/definition/versions/{versionId}/restore", { params: { path: { uid: f.id, versionId: vid } } })), {
    invalidate: [definitionKey(f.id), ["versions", f.id], ["factory", f.id], ["agents", f.id], ["automations", f.id]],
    success: `Restored version ${shortID(vid)}.`,
    onSuccess: () => navigate(fpath(f, "definition/versions")),
  });
  const active = vid === f.active_version_id;

  return (
    <Query q={v}>
      {(ver) => {
        const diffs = diffFiles(base.data?.files as Record<string, string> | undefined, (ver.files ?? {}) as Record<string, string>);
        return (
          <>
            <PageHeader
              title={`Version ${shortID(ver.id)}`}
              crumbs={[
                { label: "Factory definition", to: fpath(f, "definition") },
                { label: "Versions", to: fpath(f, "definition/versions") },
              ]}
              subtitle={
                <>
                  {ver.message || "no message"} · {ver.author} · <TimeAgo t={ver.created_at} /> {active && <Badge tone="info">active</Badge>} {!ver.valid && <Badge tone="bad">invalid</Badge>}
                </>
              }
              actions={
                canEdit &&
                f.source !== "git" &&
                !active &&
                ver.valid && (
                  <Button variant="primary" onClick={() => void confirm({ title: `Restore version ${shortID(ver.id)}?`, body: "Its files are applied as a new version.", confirmLabel: "Restore version" }).then((ok) => ok && restore.mutate(undefined))} busy={restore.isPending}>
                    Restore this version
                  </Button>
                )
              }
            />
            <ValidationList errors={ver.errors ?? undefined} title="This version failed validation" />
            <div className="toolbar">
              <label htmlFor="against">Compare against</label>
              <select id="against" value={against} onChange={(e) => setParams(e.target.value ? { against: e.target.value } : {}, { replace: true })}>
                <option value="">Empty definition</option>
                {list
                  .filter((x) => x.id !== vid)
                  .map((x) => (
                    <option key={x.id} value={x.id}>
                      {shortID(x.id)} · {x.message}
                    </option>
                  ))}
              </select>
            </div>
            {diffs.length === 0 ? <Empty title="No changes" /> : diffs.map((d) => <FileDiffView key={d.path} d={d} />)}
          </>
        );
      }}
    </Query>
  );
}
