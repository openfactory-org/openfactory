import { useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, errorMessage, unwrap, type Factory, type InboxItem } from "../api/client";
import { useToast } from "../components/Toast";
import { useAuth } from "../auth/AuthContext";
import { fpath } from "../components/FactoryLayout";
import { Inbox as InboxIcon } from "lucide-react";
import { Badge, Button, Empty, InlineText, PageHeader, Query, TimeAgo } from "../components/ui";
import { FAILURE_TEXT, humanizeError } from "../lib/format";
import { useMutate } from "../lib/mutate";
import { useFactories } from "../lib/queries";

export const INBOX_KINDS: { id: InboxItem["kind"]; label: string }[] = [
  { id: "approval", label: "Approval" },
  { id: "question", label: "Question" },
  { id: "blocked", label: "Waits for input" },
  { id: "review", label: "Review" },
  { id: "handoff", label: "Handoff" },
  { id: "failure", label: "Failure" },
  { id: "answered", label: "For your information" },
  { id: "spend_limit", label: "Spend limit" },
];

const KIND_LABEL: Record<string, string> = Object.fromEntries(INBOX_KINDS.map((k) => [k.id, k.label]));

/** The time after a resolve in which the API accepts an undo. */
const UNDO_MS = 10 * 60 * 1000;

type Answer = { question_id: string; selected?: string[]; text?: string };

/** Resolves an inbox item (approve with an option, answer a question, or mark resolved). */
export function useResolveInbox() {
  const qc = useQueryClient();
  const toast = useToast();
  return useMutate(
    ({ id, resolution, answers }: { id: string; resolution?: string; answers?: Answer[] }) =>
      unwrap(api.POST("/inbox/{id}/resolve", { params: { path: { id } }, body: answers ? { answers } : { resolution } })),
    {
      invalidate: [["inbox"], ["workitem"]],
      success: (it) => `Resolved: ${it.title}`,
      // The toast offers Undo. The API refuses it when the agent already read the answer.
      action: (it) => ({
        label: "Undo",
        onClick: () => {
          unwrap(api.POST("/inbox/{id}/unresolve", { params: { path: { id: it.id } } }))
            .then(() => Promise.all([qc.invalidateQueries({ queryKey: ["inbox"] }), qc.invalidateQueries({ queryKey: ["workitem"] })]))
            .catch((err) => toast.show(errorMessage(err), "error"));
        },
      }),
    },
  );
}

/** Undoes a resolve. The API refuses it after 10 minutes or when the run read the answer. */
export function useUnresolveInbox() {
  return useMutate((id: string) => unwrap(api.POST("/inbox/{id}/unresolve", { params: { path: { id } } })), {
    invalidate: [["inbox"], ["workitem"]],
    success: (it) => `Reopened: ${it.title}`,
  });
}

/** Sets the read state of an item for the signed-in user. */
export function useReadInbox() {
  return useMutate(
    ({ id, read }: { id: string; read: boolean }) => unwrap(api.POST("/inbox/{id}/read", { params: { path: { id } }, body: { read } })),
    { invalidate: [["inbox"]] },
  );
}

/** The text of an item. A failure shows the message for people first and keeps the raw text behind a disclosure. */
function InboxBody({ item }: { item: InboxItem }) {
  if (item.kind !== "failure")
    return (
      <div className="prose inbox-body">
        <InlineText text={item.body ?? ""} />
      </div>
    );
  const { message, detail } = humanizeError(item.body);
  if (FAILURE_TEXT[detail]) {
    return (
      <div className="inbox-body">
        <span className="prose">{message}</span> <code className="inbox-code">{detail}</code>
      </div>
    );
  }
  return (
    <div className="inbox-body">
      <div className="prose">{message}</div>
      {detail && (
        <details className="small">
          <summary className="muted">Technical details</summary>
          <pre className="code">{detail}</pre>
        </details>
      )}
    </div>
  );
}

/** One inbox item with the actions its kind allows. */
export function InboxItemCard({ item, factory, inWorkItem }: { item: InboxItem; factory?: Factory; inWorkItem?: boolean }) {
  const { me } = useAuth();
  const resolve = useResolveInbox();
  const unresolve = useUnresolveInbox();
  const read = useReadInbox();
  const [answer, setAnswer] = useState("");
  const [picked, setPicked] = useState<string[]>([]);
  const [open, setOpen] = useState(false);
  // Resolving needs the user role, like the API. An approvers list in
  // factory.yaml can still refuse an approval (403).
  const canAct = !!me && !item.resolved;
  const question = item.questions?.[0];
  const options = question?.options ?? item.options ?? [];
  const recommended = question?.recommended_option;
  const multi = question?.type === "multi_select";
  const canUndo =
    item.resolved && !!item.resolved_by && item.resolved_via !== "forge" && !!item.resolved_at && Date.now() - Date.parse(item.resolved_at) < UNDO_MS;

  function toggle() {
    setOpen((o) => !o);
    if (!open && !item.is_read && !read.isPending) read.mutate({ id: item.id, read: true });
  }

  function submitAnswer(e: FormEvent) {
    e.preventDefault();
    if (answer.trim()) resolve.mutate({ id: item.id, resolution: answer.trim() });
  }

  function submitPicked(e: FormEvent) {
    e.preventDefault();
    if (question && picked.length > 0) resolve.mutate({ id: item.id, answers: [{ question_id: question.question_id, selected: picked }] });
  }

  return (
    <li className={`inbox-item ${item.resolved ? "resolved" : ""} ${item.is_read ? "" : "unread"}`}>
      <div className="inbox-head">
        <h3 className="inbox-title">
          <button type="button" className="btn-link-plain" data-row aria-expanded={open} onClick={toggle}>
            {item.title}
          </button>
        </h3>
        {!item.is_read && !item.resolved && <span className="sr-only">Unread</span>}
        <Badge>{KIND_LABEL[item.kind] ?? item.kind}</Badge>
        <span className="muted small inbox-meta">
          {factory && !inWorkItem && (
            <>
              <Link to={fpath(factory)}>{factory.name}</Link> ·{" "}
            </>
          )}
          <TimeAgo t={item.created_at} />
        </span>
      </div>
      {(open || !item.resolved) && item.body && item.body.trim() !== item.title.trim() && <InboxBody item={item} />}
      <div className="inbox-foot">
        {item.resolved ? (
          <p className="muted small">
            Resolved{item.resolution ? `: ${item.resolution}` : ""}
            {item.resolved_by ? ` by ${item.resolved_by}` : ""}
            {item.resolved_via ? ` from ${item.resolved_via}` : ""} <TimeAgo t={item.resolved_at} />
            {canUndo && (
              <>
                {" "}
                <Button onClick={() => unresolve.mutate(item.id)} busy={unresolve.isPending}>
                  Undo
                </Button>
              </>
            )}
          </p>
        ) : (
          canAct && (
            <div className="inbox-actions">
              {item.kind === "question" && multi && question ? (
                <form onSubmit={submitPicked} className="input-row" aria-label={`Answer: ${item.title}`}>
                  {options.map((o) => (
                    <label key={o} className="check">
                      <input
                        type="checkbox"
                        checked={picked.includes(o)}
                        onChange={(e) => setPicked((p) => (e.target.checked ? [...p, o] : p.filter((x) => x !== o)))}
                      />{" "}
                      {o}
                      {o === recommended ? " (recommended)" : ""}
                    </label>
                  ))}
                  <Button type="submit" variant="primary" busy={resolve.isPending} disabled={picked.length === 0}>
                    Answer
                  </Button>
                </form>
              ) : item.kind === "question" || item.kind === "blocked" ? (
                <form onSubmit={submitAnswer} className="input-row" aria-label={`Answer: ${item.title}`}>
                  {options.map((o) => (
                    <Button
                      key={o}
                      variant={o === recommended ? "primary" : "secondary"}
                      onClick={() => resolve.mutate({ id: item.id, resolution: o })}
                      busy={resolve.isPending}
                      title={`Send "${o}" as the answer`}
                    >
                      Answer: {o === recommended ? `${o} (recommended)` : o}
                    </Button>
                  ))}
                  <label className="sr-only" htmlFor={`answer-${item.id}`}>
                    Answer
                  </label>
                  <input id={`answer-${item.id}`} value={answer} onChange={(e) => setAnswer(e.target.value)} placeholder="Type an answer…" required={options.length === 0} />
                  <Button type="submit" variant="primary" busy={resolve.isPending} disabled={!answer.trim()}>
                    Answer
                  </Button>
                </form>
              ) : item.kind === "approval" && options.length > 0 ? (
                <div className="input-row" role="group" aria-label={`Decide: ${item.title}`}>
                  {options.map((o, i) => (
                    <Button key={o} variant={i === 0 ? "primary" : "secondary"} onClick={() => resolve.mutate({ id: item.id, resolution: o })} busy={resolve.isPending}>
                      {o}
                    </Button>
                  ))}
                </div>
              ) : item.kind === "approval" ? (
                <div className="input-row" role="group" aria-label={`Decide: ${item.title}`}>
                  <Button variant="primary" onClick={() => resolve.mutate({ id: item.id, resolution: "approved" })} busy={resolve.isPending}>
                    Approve
                  </Button>
                  <Button onClick={() => resolve.mutate({ id: item.id, resolution: "rejected" })} busy={resolve.isPending}>
                    Reject
                  </Button>
                </div>
              ) : item.kind === "review" && options.length > 0 ? (
                <div className="input-row" role="group" aria-label={`Decide: ${item.title}`}>
                  {options.map((o, i) => (
                    <Button key={o} variant={i === 0 ? "primary" : "secondary"} onClick={() => resolve.mutate({ id: item.id, resolution: o })} busy={resolve.isPending}>
                      {o === "approve" ? "Approve and apply" : o === "reject" ? "Reject" : o}
                    </Button>
                  ))}
                </div>
              ) : item.kind === "answered" || item.kind === "spend_limit" ? (
                <Button onClick={() => resolve.mutate({ id: item.id, resolution: "seen" })} busy={resolve.isPending}>
                  Dismiss
                </Button>
              ) : (
                <Button onClick={() => resolve.mutate({ id: item.id, resolution: "resolved" })} busy={resolve.isPending}>
                  Mark resolved
                </Button>
              )}
            </div>
          )
        )}
        <nav className="inbox-links" aria-label={`Links for: ${item.title}`}>
          {item.run_id && <Link to={`/runs/${encodeURIComponent(item.run_id)}`}>View run</Link>}
          {item.work_item_id && factory && !inWorkItem && <Link to={fpath(factory, `work/${item.work_item_id}`)}>View work item</Link>}
          {!item.resolved && (
            <button type="button" className="btn-link-plain" onClick={() => read.mutate({ id: item.id, read: !item.is_read })}>
              {item.is_read ? "Mark as unread" : "Mark as read"}
            </button>
          )}
        </nav>
      </div>
    </li>
  );
}

/** Team inbox (/inbox) or a factory's inbox (/f/:fid/inbox). */
export function InboxPage({ factoryID }: { factoryID?: string }) {
  const [params, setParams] = useSearchParams();
  const factories = useFactories();
  const kind = params.get("kind") ?? "";
  const search = params.get("q") ?? "";
  const resolved = params.get("resolved") === "1";
  const scope = params.get("scope") ?? "mine";
  const fid = factoryID ?? params.get("factory") ?? "";
  const { canEdit, canEditFactory } = useAuth();
  // Only an editor or an admin lists the items of the whole team.
  const canSeeTeam = factoryID ? canEditFactory(factoryID) : canEdit;
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };
  const q = useQuery({
    queryKey: ["inbox", "list", { fid, kind, search, resolved, scope }],
    queryFn: () =>
      unwrap(
        api.GET("/inbox", {
          params: {
            query: {
              factory_id: fid || undefined,
              kind: kind || undefined,
              search: search || undefined,
              include_resolved: resolved || undefined,
              scope: scope === "team" && canSeeTeam ? "team" : undefined,
              limit: 200,
            },
          },
        }),
      ).then((r) => r.items ?? []),
  });
  const byID = new Map((factories.data ?? []).map((f) => [f.id, f]));

  return (
    <>
      <PageHeader title="Inbox" />
      <div className="toolbar" role="search">
        {canSeeTeam && (
          <>
            <label className="sr-only" htmlFor="scope">
              Scope
            </label>
            <select id="scope" value={scope} onChange={(e) => set("scope", e.target.value === "mine" ? "" : e.target.value)}>
              <option value="mine">Waiting on me</option>
              <option value="team">Whole team</option>
            </select>
          </>
        )}
        {!factoryID && (
          <>
            <label className="sr-only" htmlFor="ifactory">
              Factory
            </label>
            <select id="ifactory" value={fid} onChange={(e) => set("factory", e.target.value)}>
              <option value="">All factories</option>
              {(factories.data ?? []).map((f) => (
                <option key={f.id} value={f.id}>
                  {f.name}
                </option>
              ))}
            </select>
          </>
        )}
        <label className="sr-only" htmlFor="ikind">
          Kind
        </label>
        <select id="ikind" value={kind} onChange={(e) => set("kind", e.target.value)}>
          <option value="">All kinds</option>
          {INBOX_KINDS.map((k) => (
            <option key={k.id} value={k.id}>
              {k.label}
            </option>
          ))}
        </select>
        <label className="check">
          <input type="checkbox" checked={resolved} onChange={(e) => set("resolved", e.target.checked ? "1" : "")} /> Show resolved
        </label>
        <span className="spacer" />
        <label className="sr-only" htmlFor="iq">
          Search
        </label>
        <input id="iq" type="search" placeholder="Search the inbox…" defaultValue={search} onKeyDown={(e) => e.key === "Enter" && set("q", (e.target as HTMLInputElement).value)} onBlur={(e) => set("q", e.target.value)} />
      </div>
      <Query q={q}>
        {(items) => {
          const sorted = [...items].sort((a, b) => Number(a.resolved) - Number(b.resolved) || (b.created_at ?? "").localeCompare(a.created_at ?? ""));
          return sorted.length === 0 ? (
            <Empty icon={<InboxIcon size={18} />} title="You're all caught up">
              Nothing is waiting on a person right now.
            </Empty>
          ) : (
            <ul className="list bleed inbox-list" aria-label="Inbox items">
              {sorted.map((it) => (
                <InboxItemCard key={it.id} item={it} factory={byID.get(it.factory_id ?? "")} />
              ))}
            </ul>
          );
        }}
      </Query>
    </>
  );
}
