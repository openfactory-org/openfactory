import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { Plus, Server } from "lucide-react";
import { api, errorMessage, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { DeadEvents } from "../components/DeadEvents";
import { confirm, Dialog } from "../components/overlays";
import { Alert, Badge, Button, Empty, Field, PageHeader, Query, TimeAgo } from "../components/ui";
import { kvLines, parseKVLines } from "../lib/format";
import { useFactories } from "../lib/queries";
import { useMutate } from "../lib/mutate";

type Worker = Schemas["Worker"];

export const WORKERS_KEY = ["workers"];

const STATE_TEXT: Record<NonNullable<Worker["status"]>, string> = {
  pending: "Pending",
  online: "Online",
  offline: "Offline",
  draining: "Draining",
  revoked: "Revoked",
};
const STATE_TONE: Record<NonNullable<Worker["status"]>, string> = {
  pending: "neutral",
  online: "ok",
  offline: "neutral",
  draining: "warn",
  revoked: "bad",
};
const KIND_TEXT: Record<NonNullable<Worker["kind"]>, string> = { microvm: "microVM host", process: "Process (development)" };

/** The version of the runtime on the host: the reported fact, else the worker version. */
function runtimeVersion(w: Worker): string {
  const v = w.facts?.runtime_version;
  return (typeof v === "string" && v) || w.version || "";
}

/** Admin page: enroll hosts and manage the workers of the team. */
export function WorkersPage() {
  const { isAdmin } = useAuth();
  const factories = useFactories();
  const [enrolling, setEnrolling] = useState(false);
  const [editing, setEditing] = useState<Worker | null>(null);
  const q = useQuery({ queryKey: WORKERS_KEY, queryFn: () => unwrap(api.GET("/workers")).then((r) => r.items ?? []), refetchInterval: 30_000 });
  const act = useMutate(
    (v: { w: Worker; op: "drain" | "resume" | "revoke" }) => {
      const params = { params: { path: { id: v.w.id ?? "" } } };
      if (v.op === "drain") return unwrap(api.POST("/workers/{id}/drain", params));
      if (v.op === "resume") return unwrap(api.POST("/workers/{id}/resume", params));
      return unwrap(api.POST("/workers/{id}/revoke", params));
    },
    { invalidate: [WORKERS_KEY], success: (_, v) => ({ drain: "Worker is draining.", resume: "Worker takes new sessions again.", revoke: "Worker revoked." })[v.op] },
  );
  const del = useMutate((w: Worker) => unwrap(api.DELETE("/workers/{id}", { params: { path: { id: w.id ?? "" } } })), { invalidate: [WORKERS_KEY], success: "Worker deleted." });
  const factoryName = (id: string) => (factories.data ?? []).find((f) => f.id === id)?.name ?? id;

  return (
    <>
      <PageHeader
        title="Workers"
        subtitle="Hosts that run agent sessions in microVMs, enrolled by an admin. Provider sandboxes are not listed here."
        actions={
          isAdmin && (
            <Button variant="primary" onClick={() => setEnrolling(true)}>
              <Plus size={14} aria-hidden="true" /> Enroll worker
            </Button>
          )
        }
      />
      <Query q={q}>
        {(ws) =>
          ws.length === 0 ? (
            <Empty
              icon={<Server size={18} />}
              title="No workers have enrolled"
              action={
                isAdmin && (
                  <Button variant="primary" onClick={() => setEnrolling(true)}>
                    Enroll worker
                  </Button>
                )
              }
            >
              Runs use the sandbox provider of the factory. To run sessions on your own host, create an enrollment token and run the command on the host.
            </Empty>
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th scope="col">Worker</th>
                    <th scope="col">Kind</th>
                    <th scope="col">State</th>
                    <th scope="col">Slots</th>
                    <th scope="col">Labels</th>
                    <th scope="col">Factories</th>
                    <th scope="col">Runtime version</th>
                    <th scope="col">Last seen</th>
                    <th scope="col">
                      <span className="sr-only">Actions</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {ws.map((w) => {
                    const state = w.status ?? "offline";
                    const total = w.slots ?? w.capacity ?? 0;
                    const used = w.slots_used ?? w.active ?? 0;
                    return (
                      <tr key={w.id}>
                        <td>
                          {w.name}
                          <br />
                          <code className="small muted">{w.id}</code>
                        </td>
                        <td>{w.kind ? KIND_TEXT[w.kind] : (w.provider ?? "")}</td>
                        <td>
                          <Badge tone={STATE_TONE[state]}>{STATE_TEXT[state]}</Badge>
                        </td>
                        <td>
                          <meter min={0} max={total || 1} value={used} aria-label={`Slots used by ${w.name}`} /> {used}/{total}
                        </td>
                        <td>
                          {Object.entries(w.labels ?? {}).length === 0 ? (
                            <span className="muted">—</span>
                          ) : (
                            Object.entries(w.labels ?? {}).map(([k, v]) => (
                              <span key={k} className="mono small">
                                {k}={v}{" "}
                              </span>
                            ))
                          )}
                        </td>
                        <td>{(w.factories ?? []).length === 0 ? <span className="muted">All factories</span> : (w.factories ?? []).map(factoryName).join(", ")}</td>
                        <td className="muted">{runtimeVersion(w)}</td>
                        <td>
                          <TimeAgo t={w.last_seen ?? w.last_heartbeat} />
                        </td>
                        <td className="right">
                          {isAdmin && state !== "revoked" && (
                            <>
                              <Button variant="ghost" onClick={() => setEditing(w)} aria-label={`Edit factories of ${w.name}`}>
                                Factories
                              </Button>
                              {state === "draining" ? (
                                <Button variant="ghost" onClick={() => act.mutate({ w, op: "resume" })} aria-label={`Resume ${w.name}`}>
                                  Resume
                                </Button>
                              ) : (
                                <Button variant="ghost" onClick={() => act.mutate({ w, op: "drain" })} aria-label={`Drain ${w.name}`}>
                                  Drain
                                </Button>
                              )}
                              <Button
                                variant="ghost"
                                aria-label={`Revoke ${w.name}`}
                                onClick={() =>
                                  void confirm({
                                    title: `Revoke worker ${w.name}?`,
                                    body: "The worker disconnects and cannot connect again. Sessions on it stop.",
                                    confirmLabel: "Revoke worker",
                                    danger: true,
                                  }).then((ok) => ok && act.mutate({ w, op: "revoke" }))
                                }
                              >
                                Revoke
                              </Button>
                            </>
                          )}
                          {isAdmin && (
                            <Button
                              variant="ghost"
                              aria-label={`Delete ${w.name}`}
                              onClick={() => void confirm({ title: `Delete worker ${w.name}?`, body: "The worker is revoked and its record is removed. You cannot undo this.", confirmLabel: "Delete worker", danger: true }).then((ok) => ok && del.mutate(w))}
                            >
                              Delete
                            </Button>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )
        }
      </Query>
      {isAdmin && <EnrollDialog open={enrolling} onClose={() => setEnrolling(false)} />}
      {isAdmin && <FactoriesDialog worker={editing} onClose={() => setEditing(null)} />}
      <DeadEvents />
    </>
  );
}

/** Factory restriction picker: nothing selected means all factories of the team. */
function FactoryPicker({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  const factories = useFactories();
  return (
    <fieldset className="stack stack-sm">
      <legend className="small muted">Factories (none selected means all factories of the team)</legend>
      {(factories.data ?? []).map((f) => (
        <label key={f.id} className="check">
          <input type="checkbox" checked={value.includes(f.id)} onChange={(e) => onChange(e.target.checked ? [...value, f.id] : value.filter((x) => x !== f.id))} /> {f.name}
        </label>
      ))}
    </fieldset>
  );
}

function EnrollDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [form, setForm] = useState({ name: "", labels: "", factories: [] as string[] });
  const [error, setError] = useState<string | null>(null);
  const [created, setCreated] = useState<Schemas["WorkerEnrollment"] | null>(null);
  const create = useMutate((body: Schemas["WorkerEnrollmentRequest"]) => unwrap(api.POST("/workers/enrollments", { body })), {
    invalidate: [WORKERS_KEY],
    quietErrors: true,
    onSuccess: (r) => setCreated(r),
  });

  function close() {
    setCreated(null);
    setError(null);
    setForm({ name: "", labels: "", factories: [] });
    onClose();
  }
  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    let labels: Record<string, string>;
    try {
      labels = parseKVLines(form.labels);
    } catch (err) {
      return setError(`Labels: ${errorMessage(err)}`);
    }
    try {
      await create.mutateAsync({ name: form.name || undefined, labels, factories: form.factories, expires_in_seconds: 3600 });
    } catch (err) {
      setError(errorMessage(err));
    }
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !o && close()} title="Enroll worker" description="Creates a single-use token that is valid for one hour.">
      {created ? (
        <div className="stack stack-sm">
          <Alert kind="success">The token is shown once. Copy it now.</Alert>
          <Field label="Enrollment token" htmlFor="we-token">
            <input id="we-token" className="mono" readOnly value={created.token} onFocus={(e) => e.currentTarget.select()} />
          </Field>
          {created.command && (
            <Field label="Command to run on the host" htmlFor="we-command">
              <textarea id="we-command" className="mono" rows={3} readOnly value={created.command} onFocus={(e) => e.currentTarget.select()} />
            </Field>
          )}
          <p className="small muted">
            Expires <TimeAgo t={created.expires_at} />.
          </p>
          <div className="dialog-footer">
            <Button variant="primary" onClick={close}>
              Done
            </Button>
          </div>
        </div>
      ) : (
        <form className="stack stack-sm" onSubmit={submit} autoComplete="off">
          {error && <Alert kind="error">{error}</Alert>}
          <Field label="Name" htmlFor="we-name" hint="Optional. The host can send a name too.">
            <input id="we-name" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="build-host-1" />
          </Field>
          <Field label="Labels" htmlFor="we-labels" hint="One key=value per line. An agent can select workers with workerHost: pool:<label>.">
            <textarea id="we-labels" rows={3} className="mono" value={form.labels} onChange={(e) => setForm({ ...form, labels: e.target.value })} placeholder="pool=linux" />
          </Field>
          <FactoryPicker value={form.factories} onChange={(factories) => setForm({ ...form, factories })} />
          <div className="dialog-footer">
            <Button onClick={close}>Cancel</Button>
            <Button type="submit" variant="primary" busy={create.isPending}>
              Create token
            </Button>
          </div>
        </form>
      )}
    </Dialog>
  );
}

/** Edits the factory restriction of a worker (PATCH /workers/{id}). */
function FactoriesDialog({ worker, onClose }: { worker: Worker | null; onClose: () => void }) {
  const [picked, setPicked] = useState<string[] | null>(null);
  const value = picked ?? worker?.factories ?? [];
  const save = useMutate((v: { id: string; factories: string[] }) => unwrap(api.PATCH("/workers/{id}", { params: { path: { id: v.id } }, body: { factories: v.factories } })), {
    invalidate: [WORKERS_KEY],
    success: "Worker updated.",
    onSuccess: () => close(),
  });
  function close() {
    setPicked(null);
    onClose();
  }
  return (
    <Dialog open={!!worker} onOpenChange={(o) => !o && close()} title={`Factories of ${worker?.name ?? "worker"}`} description="Restrict the worker to some factories. With none selected, every factory of the team can use it.">
      <form
        className="stack stack-sm"
        onSubmit={(e) => {
          e.preventDefault();
          if (worker?.id) save.mutate({ id: worker.id, factories: value });
        }}
      >
        <FactoryPicker value={value} onChange={setPicked} />
        {worker?.labels && Object.keys(worker.labels).length > 0 && <p className="small muted mono">{kvLines(worker.labels).replace(/\n/g, ", ")}</p>}
        <div className="dialog-footer">
          <Button onClick={close}>Cancel</Button>
          <Button type="submit" variant="primary" busy={save.isPending}>
            Save
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
