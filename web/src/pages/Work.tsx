import { useState, type ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import {
  ArrowRight,
  Bot,
  Check,
  CircleDot,
  CornerDownRight,
  ExternalLink,
  GitMerge,
  GitPullRequest,
  GitPullRequestClosed,
  Link2,
  MessageSquare,
  Play,
  RotateCcw,
  ShieldCheck,
  Square,
  StickyNote,
  User,
  X,
} from "lucide-react";
import { api, unwrap, type Run, type Schemas, type WorkItem } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { fpath, useFactory } from "../components/FactoryLayout";
import { confirm } from "../components/overlays";
import { useShell } from "../components/shell";
import { Button, Empty, ErrorAlert, Keys, PageHeader, Query, Segmented, Skeleton, StateBadge, StatusIcon, TimeAgo } from "../components/ui";
import { firstLine, humanizeError, money, shortID, stateLabel } from "../lib/format";
import { useMutate } from "../lib/mutate";
import { isSubmitChord, MOD } from "../lib/queries";
import { InboxItemCard } from "./Inbox";

export const STAGES = ["intake", "triage", "planning", "awaiting_approval", "building", "reviewing", "handoff", "complete", "cancelled"] as const;
const OPEN_STAGES = STAGES.filter((s) => s !== "complete" && s !== "cancelled");
/** The order of the groups in the list: work that waits on a person first, finished work last. */
const GROUP_ORDER = ["awaiting_approval", "handoff", "reviewing", "building", "planning", "triage", "intake", "complete", "cancelled"];
/** The stages of the usual path, shown as steps on a work item. */
const PATH = ["intake", "triage", "planning", "building", "reviewing", "handoff", "complete"];

function stageLabel(s?: string | null): string {
  return (s ?? "").replace(/_/g, " ");
}

type PR = NonNullable<WorkItem["pull_requests"]>[number];

function PRIcon({ state }: { state?: string }) {
  const s = (state ?? "").toLowerCase();
  if (s === "merged") return <GitMerge size={14} aria-hidden="true" />;
  if (s === "closed") return <GitPullRequestClosed size={14} aria-hidden="true" />;
  return <GitPullRequest size={14} aria-hidden="true" />;
}

function PRChip({ pr }: { pr: PR }) {
  return (
    <a href={pr.url} className={`chip pr-${(pr.state ?? "open").toLowerCase()}`} onClick={(e) => e.stopPropagation()}>
      <PRIcon state={pr.state} />#{pr.number} {pr.state}
    </a>
  );
}

function WorkRow({ wi, fid, showStage = true }: { wi: WorkItem; fid: string; showStage?: boolean }) {
  const src = wi.source;
  const ref = src?.repo && src.number ? `${src.repo}#${src.number}` : src?.external_id || src?.provider;
  return (
    <li>
      <div className="list-row">
        <StatusIcon state={wi.stage} />
        <Link to={fpath(fid, `work/${wi.id}`)} className="row-title" data-row style={{ color: "inherit" }}>
          {wi.title || shortID(wi.id)}
        </Link>
        <span className="row-meta">
          {(wi.pull_requests ?? []).map((pr) => (
            <PRChip key={`${pr.repo}#${pr.number}`} pr={pr} />
          ))}
          {showStage && <span className="hide-sm state-label">{stageLabel(wi.stage)}</span>}
          {ref && (
            <span className="hide-sm faint" title={src?.provider}>
              {ref}
            </span>
          )}
        </span>
        <span className="row-time">
          <TimeAgo t={wi.updated_at ?? wi.created_at} />
        </span>
      </div>
    </li>
  );
}

/** Work items as dense rows. With `grouped`, the rows are grouped by stage. */
export function WorkTable({ items, fid, grouped, empty }: { items: WorkItem[]; fid: string; grouped?: boolean; empty?: ReactNode }) {
  if (items.length === 0) return <>{empty ?? <Empty icon={<CircleDot size={18} />} title="No work items yet">Send the foreman a request to get started.</Empty>}</>;
  if (!grouped) {
    return (
      <ul className="list bleed" aria-label="Work items">
        {items.map((wi) => (
          <WorkRow key={wi.id} wi={wi} fid={fid} />
        ))}
      </ul>
    );
  }
  const groups = GROUP_ORDER.map((stage) => ({ stage, rows: items.filter((wi) => wi.stage === stage) })).filter((g) => g.rows.length > 0);
  const other = items.filter((wi) => !GROUP_ORDER.includes(wi.stage ?? ""));
  if (other.length > 0) groups.push({ stage: "other", rows: other });
  return (
    <div className="bleed">
      {groups.map((g) => (
        <section key={g.stage} aria-label={stageLabel(g.stage)}>
          <h2 className="list-group">
            <StatusIcon state={g.stage} />
            <span className="state-label">{stageLabel(g.stage)}</span>
            <span className="count">{g.rows.length}</span>
          </h2>
          <ul className="list">
            {g.rows.map((wi) => (
              <WorkRow key={wi.id} wi={wi} fid={fid} showStage={false} />
            ))}
          </ul>
        </section>
      ))}
    </div>
  );
}

const STATUS_OPTIONS = [
  { id: "", label: "All" },
  { id: "open", label: "Open" },
  { id: "closed", label: "Closed" },
];

export function WorkItemsPage() {
  const f = useFactory();
  const shell = useShell();
  const [params, setParams] = useSearchParams();
  const stage = params.get("stage") ?? "";
  const status = params.get("status") ?? "";
  const creator = params.get("creator") ?? "";
  const search = params.get("q") ?? "";
  const since = params.get("since") ?? "";
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };
  const filtered = !!(stage || status || creator || search || since);
  const users = useQuery({ queryKey: ["users"], queryFn: () => unwrap(api.GET("/users")).then((r) => r.items ?? []) });
  const q = useInfiniteQuery({
    queryKey: ["workitems", f.id, { stage, status, creator, search, since }],
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      unwrap(
        api.GET("/factories/{uid}/work-items", {
          params: {
            path: { uid: f.id },
            query: {
              stage: stage || (status === "open" ? OPEN_STAGES.join(",") : status === "closed" ? "complete,cancelled" : undefined),
              creator: creator || undefined,
              search: search || undefined,
              since: since || undefined,
              limit: 100,
              cursor: pageParam || undefined,
            },
          },
        }),
      ),
    getNextPageParam: (last) => (last.page_info?.has_next_page ? last.page_info.next_cursor : undefined),
  });
  const items = (q.data?.pages ?? []).flatMap((p) => p.items ?? []);
  return (
    <>
      <PageHeader
        title="Work items"
        crumbs={[{ label: f.name, to: fpath(f) }]}
        actions={
          <Button variant="primary" onClick={() => shell.openComposer(f.id)}>
            New work item
          </Button>
        }
      />
      <div className="toolbar" role="search">
        <Segmented label="Status" value={status} onChange={(v) => set("status", v)} options={STATUS_OPTIONS} />
        <label className="sr-only" htmlFor="wstage">
          Stage
        </label>
        <select id="wstage" value={stage} onChange={(e) => set("stage", e.target.value)}>
          <option value="">Any stage</option>
          {STAGES.map((s) => (
            <option key={s} value={s}>
              {/* Sentence case, like the group headings of the board (.state-label). */}
              {stateLabel(s)}
            </option>
          ))}
        </select>
        <label className="sr-only" htmlFor="wcreator">
          Creator
        </label>
        <select id="wcreator" value={creator} onChange={(e) => set("creator", e.target.value)}>
          <option value="">Anyone</option>
          <option value="me">Me</option>
          {(users.data ?? []).map((u) => (
            <option key={u.id} value={u.id}>
              {u.name || u.email}
            </option>
          ))}
        </select>
        <label className="sr-only" htmlFor="wsince">
          Since
        </label>
        <input id="wsince" type="date" value={since} onChange={(e) => set("since", e.target.value)} />
        <span className="spacer" />
        <label className="sr-only" htmlFor="wq">
          Search
        </label>
        <input id="wq" type="search" placeholder="Search titles…" defaultValue={search} onKeyDown={(e) => e.key === "Enter" && set("q", (e.target as HTMLInputElement).value)} onBlur={(e) => set("q", e.target.value)} />
      </div>
      {q.isLoading ? (
        <Skeleton rows={6} />
      ) : q.isError ? (
        <ErrorAlert error={q.error} />
      ) : (
        <>
          <WorkTable
            items={items}
            fid={f.id}
            grouped
            empty={
              filtered ? (
                <Empty title="No work items match">Change or clear the filters.</Empty>
              ) : (
                <Empty
                  icon={<CircleDot size={18} />}
                  title="No work items yet"
                  action={
                    <Button variant="primary" onClick={() => shell.openComposer(f.id)}>
                      New work item <Keys k="C" />
                    </Button>
                  }
                >
                  Send the foreman a request. It becomes a work item that you can follow here.
                </Empty>
              )
            }
          />
          {q.hasNextPage && (
            <div className="form-actions" style={{ justifyContent: "center" }}>
              <Button onClick={() => void q.fetchNextPage()} busy={q.isFetchingNextPage}>
                Load more
              </Button>
            </div>
          )}
        </>
      )}
    </>
  );
}

/** Orders runs depth-first under their parents. */
export function runTree(runs: Run[], opts: { newestFirst?: boolean } = {}): { run: Run; depth: number }[] {
  const ids = new Set(runs.map((r) => r.id ?? r.run_id));
  const byParent = new Map<string, Run[]>();
  for (const r of runs) {
    const p = r.parent_run_id && ids.has(r.parent_run_id) ? r.parent_run_id : "";
    byParent.set(p, [...(byParent.get(p) ?? []), r]);
  }
  // Children read in order of start. Top-level runs come newest first on a run list.
  byParent.forEach((list, parent) =>
    list.sort((a, b) => (parent === "" && opts.newestFirst ? -1 : 1) * (a.created_at ?? "").localeCompare(b.created_at ?? "")),
  );
  const out: { run: Run; depth: number }[] = [];
  const walk = (parent: string, depth: number) => {
    for (const r of byParent.get(parent) ?? []) {
      out.push({ run: r, depth });
      if (depth < 20) walk(r.run_id, depth + 1);
    }
  };
  walk("", 0);
  return out;
}

type TimelineEvent = Schemas["WorkItemEvent"];

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

/** Maps a user ID to a name for the timeline. */
type NameOf = (userID: string) => string | undefined;

/** What the timeline knows about the people behind its events. */
type Actors = {
  nameOf?: NameOf;
  /** The creator of a run of the work item, by run ID. */
  runCreator?: (runID: string) => Schemas["Principal"] | undefined;
  /** The user who created the work item. */
  itemCreatorID?: string;
  /** Maps an automation ID to its name. */
  automationName?: (automationID: string) => string | undefined;
};

// Platform actors of automations: "<prefix>:<automation ID>".
const AUTOMATION_ACTORS = ["automation:", "schedule:", "webhook:"];

// The engine sends the resolution of an inbox item to the run as a message
// that starts with one of these markers. The timeline already shows the
// answer or decision, so the message is not shown again.
const FOLLOW_UP_MARKER = /^\[human (answer|decision|response|correction)\]/;

/** Reports whether a timeline event is the platform's copy of an inbox resolution. */
export function isInboxFollowUp(e: TimelineEvent): boolean {
  const d = (e.data ?? {}) as Record<string, unknown>;
  return e.kind === "message_in" && FOLLOW_UP_MARKER.test(str(d.text));
}

// Channel actors: a person acted through the dashboard, the API, the CLI
// or MCP, and the event does not name the person.
const CHANNEL_ACTORS = new Set(["dashboard", "api", "cli", "mcp"]);

function personName(userID: string, actors: Actors): string {
  return actors.nameOf?.(userID) ?? "A team member";
}

function actorName(e: TimelineEvent, actors: Actors = {}): string {
  const a = e.actor ?? "";
  if (!a || a === "system") return "The factory";
  if (a.startsWith("run:")) return "The foreman";
  if (a.startsWith("user:")) return personName(a.slice(5), actors);
  const auto = AUTOMATION_ACTORS.find((p) => a.startsWith(p));
  if (auto) {
    const name = actors.automationName?.(a.slice(auto.length));
    return name ? `The automation ${name}` : "An automation";
  }
  if (CHANNEL_ACTORS.has(a.toLowerCase())) {
    // A run records who started it: name that person.
    const p = e.run_id ? actors.runCreator?.(e.run_id) : undefined;
    if (p?.type === "user") return personName(p.id, actors);
    if (p) return p.name || p.id;
    // The first stage change is the creation of the work item.
    const d = (e.data ?? {}) as Record<string, unknown>;
    if (e.kind === "stage_changed" && !d.from && actors.itemCreatorID) return personName(actors.itemCreatorID, actors);
    return "A team member";
  }
  return a;
}

/** One timeline event as a sentence, an icon and an optional block of text. */
export function describeEvent(e: TimelineEvent, actors: Actors = {}): { icon: ReactNode; text: ReactNode; body?: string } {
  const d = (e.data ?? {}) as Record<string, unknown>;
  const who = <strong>{actorName(e, actors)}</strong>;
  const size = 13;
  switch (e.kind) {
    case "stage_changed": {
      const to = stageLabel(str(d.to) || str(d.stage));
      const from = stageLabel(str(d.from));
      return {
        icon: <ArrowRight size={size} />,
        text: (
          <>
            {who} moved this {from ? `from ${from} ` : ""}to <strong>{to || "a new stage"}</strong>
          </>
        ),
        body: str(d.note),
      };
    }
    case "run_started":
      return { icon: <Play size={size} />, text: <>{who} started {str(d.name) || str(d.agent) ? <strong>{str(d.name) || str(d.agent)}</strong> : "a run"}</> };
    case "side_run_started":
      return { icon: <CornerDownRight size={size} />, text: <>An automation started {str(d.agent) ? <strong>{str(d.agent)}</strong> : "a side run"}</> };
    case "run_finished": {
      const state = str(d.state).toLowerCase();
      return {
        icon: state === "succeeded" ? <Check size={size} /> : state === "cancelled" ? <Square size={size} /> : <X size={size} />,
        text: (
          <>
            <strong>{str(d.name) || "The run"}</strong>{" "}
            {state === "succeeded" ? "finished its turn" : state === "blocked" ? "is waiting on a person" : state ? `ended: ${state}` : "ended"}
          </>
        ),
        body: humanizeError(str(d.error) || str(d.message) || str(d.text)).message,
      };
    }
    case "message_in":
      return { icon: <User size={size} />, text: <>{who} wrote</>, body: str(d.text) };
    case "message_out":
      return { icon: <Bot size={size} />, text: <>{who} {str(d.kind) === "handoff" ? "handed off" : str(d.kind) === "error" ? "reported a problem" : "posted an update"}</>, body: str(d.text) };
    case "pr_linked":
      return {
        icon: <GitPullRequest size={size} />,
        text: (
          <>
            {who} linked pull request {str(d.url) ? <a href={str(d.url)}>{str(d.repo) ? `${str(d.repo)}#${String(d.number ?? "")}` : str(d.url)}</a> : null}
          </>
        ),
      };
    case "issue_linked":
      return { icon: <Link2 size={size} />, text: <>{who} linked issue <strong>{str(d.key) || str(d.issue)}</strong></> };
    case "reopened":
      return { icon: <RotateCcw size={size} />, text: <>{who} opened this again</> };
    case "sender_checked":
      return { icon: <ShieldCheck size={size} />, text: <>{who} checked the sender</> };
    case "note": {
      const title = str(d.title);
      const verb = str(d.kind) === "handoff" ? "handed off" : "noted";
      return {
        icon: <StickyNote size={size} />,
        text: (
          <>
            {who} {verb}
            {title ? `: ${title}` : ""}
          </>
        ),
        body: str(d.text) || str(d.summary) || str(d.body),
      };
    }
    default: {
      const body = str(d.body) || str(d.text) || str(d.resolution) || str(d.question) || str(d.summary) || str(d.note) || str(d.message);
      return {
        icon: <MessageSquare size={size} />,
        text: (
          <>
            {who} · <span className="timeline-kind">{(e.kind ?? "event").replace(/_/g, " ")}</span>
            {str(d.title) ? `: ${str(d.title)}` : ""}
          </>
        ),
        body,
      };
    }
  }
}

function TimelineItem({ e, actors }: { e: TimelineEvent; actors: Actors }) {
  const { icon, text, body } = describeEvent(e, actors);
  return (
    <li>
      <span className="tl-icon" aria-hidden="true">
        {icon}
      </span>
      <div className="tl-body">
        <div className="tl-head">
          {text}
          {" · "}
          <TimeAgo t={e.created_at} />
          {e.run_id && (
            <>
              {" · "}
              <Link to={`/runs/${encodeURIComponent(e.run_id)}`}>View run</Link>
            </>
          )}
        </div>
        {body && <div className="tl-text">{body}</div>}
      </div>
    </li>
  );
}

function StageSteps({ stage }: { stage?: string | null }) {
  const s = stage ?? "";
  if (s === "cancelled") return null;
  const at = s === "awaiting_approval" ? PATH.indexOf("planning") : PATH.indexOf(s);
  return (
    <ol className="stepper" aria-label="Stages">
      {PATH.map((p, i) => (
        <li key={p} className={i < at ? "done" : i === at ? "current" : ""} aria-current={i === at ? "step" : undefined}>
          <StatusIcon state={i < at ? "complete" : i === at ? s : "intake"} size={12} />
          {i === at ? stageLabel(s) : stageLabel(p)}
        </li>
      ))}
    </ol>
  );
}

export function WorkItemPage() {
  const f = useFactory();
  const { wid = "" } = useParams();
  // Cancel and complete need the user role, like the API.
  const { me } = useAuth();
  const q = useQuery({
    queryKey: ["workitem", wid],
    queryFn: () => unwrap(api.GET("/work-items/{id}", { params: { path: { id: wid } } })),
  });
  const users = useQuery({ queryKey: ["users"], queryFn: () => unwrap(api.GET("/users")).then((r) => r.items ?? []), retry: false });
  const nameOf: NameOf = (id) => {
    if (me && id === me.user.id) return "You";
    const u = (users.data ?? []).find((x) => x.id === id);
    return u ? u.name || u.email : undefined;
  };
  const automations = useQuery({
    queryKey: ["automations", f.id],
    queryFn: () => unwrap(api.GET("/factories/{uid}/automations", { params: { path: { uid: f.id } } })).then((r) => r.items ?? []),
    retry: false,
  });
  const automationName = (id: string) => {
    const a = (automations.data ?? []).find((x) => x.id === id);
    return a ? a.display_name || a.name : undefined;
  };
  const [message, setMessage] = useState("");
  const send = useMutate(
    (msg: string) => unwrap(api.POST("/work-items/{id}/messages", { params: { path: { id: wid } }, body: { message: msg } })),
    { invalidate: [["workitem", wid]], success: "Message sent to the foreman.", onSuccess: () => setMessage("") },
  );
  const cancel = useMutate(() => unwrap(api.POST("/work-items/{id}/cancel", { params: { path: { id: wid } }, body: {} })), {
    invalidate: [["workitem", wid], ["workitems", f.id]],
    success: "Work item cancelled.",
  });
  const complete = useMutate(() => unwrap(api.POST("/work-items/{id}/complete", { params: { path: { id: wid } }, body: {} })), {
    invalidate: [["workitem", wid], ["workitems", f.id]],
    success: "Work item marked complete.",
  });
  const submit = () => {
    if (message.trim() && !send.isPending) send.mutate(message.trim());
  };

  return (
    <Query q={q}>
      {(wi) => {
        const terminal = wi.stage === "complete" || wi.stage === "cancelled";
        const pending = (wi.inbox ?? []).filter((i) => !i.resolved);
        const cost = (wi.runs ?? []).reduce((a, r) => a + (r.usage?.cost_usd ?? 0), 0);
        return (
          <>
            <PageHeader
              display
              title={wi.title || shortID(wi.id)}
              crumbs={[
                { label: f.name, to: fpath(f) },
                { label: "Work items", to: fpath(f, "work") },
              ]}
              subtitle={<StageSteps stage={wi.stage} />}
              actions={
                !!me &&
                !terminal && (
                  <>
                    <Button onClick={() => complete.mutate(undefined)} busy={complete.isPending}>
                      <Check size={14} aria-hidden="true" /> Mark complete
                    </Button>
                    <Button
                      variant="danger"
                      onClick={async () => {
                        if (await confirm({ title: "Cancel this work item?", body: "The factory stops its runs. You cannot undo this.", confirmLabel: "Cancel work item", cancelLabel: "Keep working", danger: true }))
                          cancel.mutate(undefined);
                      }}
                      busy={cancel.isPending}
                    >
                      Cancel
                    </Button>
                  </>
                )
              }
            />
            <div className="two-col">
              <div className="stack">
                {pending.length > 0 && (
                  <section aria-label="Waiting on a person" className="stack stack-sm">
                    <h2 className="section-title">Waiting on a person</h2>
                    <ul className="stack stack-sm">
                      {pending.map((it) => (
                        <InboxItemCard key={it.id} item={it} factory={f} inWorkItem />
                      ))}
                    </ul>
                  </section>
                )}
                <section aria-label="Activity" className="stack">
                  <h2 className="section-title">Activity</h2>
                  {(wi.timeline ?? []).length === 0 ? (
                    <p className="muted small">No events yet.</p>
                  ) : (
                    <ol className="timeline">
                      {wi.timeline.filter((e) => !isInboxFollowUp(e)).map((e) => (
                        <TimelineItem
                          key={e.id}
                          e={e}
                          actors={{ nameOf, automationName, itemCreatorID: wi.creator_id, runCreator: (id) => (wi.runs ?? []).find((r) => r.run_id === id)?.creator }}
                        />
                      ))}
                    </ol>
                  )}
                  {!terminal && (
                    <form
                      className="reply"
                      aria-label="Message the foreman"
                      onSubmit={(e) => {
                        e.preventDefault();
                        submit();
                      }}
                    >
                      <label className="sr-only" htmlFor="wmsg">
                        Message
                      </label>
                      <textarea
                        id="wmsg"
                        rows={3}
                        value={message}
                        onChange={(e) => setMessage(e.target.value)}
                        onKeyDown={(e) => isSubmitChord(e) && submit()}
                        placeholder={`Message @${f.alias || "foreman"}: add context or steer the work…`}
                        required
                      />
                      <div className="reply-foot">
                        <Button type="submit" variant="primary" size="sm" busy={send.isPending} disabled={!message.trim()}>
                          Send <Keys k={`${MOD} ↵`} />
                        </Button>
                      </div>
                    </form>
                  )}
                </section>
              </div>
              <aside className="props" aria-label="Work item details">
                <section className="props-section">
                  <h2>Details</h2>
                  <dl className="kv">
                    <div className="kv-row">
                      <dt>Stage</dt>
                      <dd>
                        <StateBadge state={wi.stage} />
                      </dd>
                    </div>
                    <div className="kv-row">
                      <dt>Created</dt>
                      <dd>
                        <TimeAgo t={wi.created_at} />
                      </dd>
                    </div>
                    <div className="kv-row">
                      <dt>Source</dt>
                      <dd>
                        {wi.source?.url ? (
                          <a href={wi.source.url}>
                            {wi.source.title ? firstLine(wi.source.title, 40) : wi.source.repo && wi.source.number ? `${wi.source.repo}#${wi.source.number}` : firstLine(wi.source.url, 40)} <ExternalLink size={11} aria-hidden="true" />
                          </a>
                        ) : (
                          wi.source?.provider || firstLine(wi.source?.title, 40) || "—"
                        )}
                      </dd>
                    </div>
                    <div className="kv-row">
                      <dt>Cost</dt>
                      <dd className="num-inline">{money(cost)}</dd>
                    </div>
                  </dl>
                </section>
                <section className="props-section">
                  <h2>Runs</h2>
                  <ul className="run-tree">
                    {runTree(wi.runs ?? []).map(({ run, depth }) => (
                      <li key={run.run_id} style={{ paddingLeft: `${depth * 16}px` }}>
                        <StatusIcon state={run.state} />
                        <Link to={`/runs/${encodeURIComponent(run.run_id)}`}>{run.name || run.agent_name || shortID(run.run_id)}</Link>
                        <span className="sr-only">{run.state}</span>
                        <span className="cost">{money(run.usage?.cost_usd)}</span>
                      </li>
                    ))}
                    {(wi.runs ?? []).length === 0 && <li className="muted small">No runs yet.</li>}
                  </ul>
                </section>
                <section className="props-section">
                  <h2>Pull requests</h2>
                  {(wi.pull_requests ?? []).length === 0 ? (
                    <p className="muted small">None yet.</p>
                  ) : (
                    <ul className="pr-list">
                      {wi.pull_requests!.map((pr) => (
                        <li key={`${pr.repo}#${pr.number}`} className={`pr-${(pr.state ?? "open").toLowerCase()}`}>
                          <PRIcon state={pr.state} />
                          <a href={pr.url} className="truncate">
                            {pr.repo}#{pr.number}
                          </a>
                          <span className="muted small">{pr.state}</span>
                        </li>
                      ))}
                    </ul>
                  )}
                </section>
              </aside>
            </div>
          </>
        );
      }}
    </Query>
  );
}
