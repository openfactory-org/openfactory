import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, Bot, ChevronRight, Square } from "lucide-react";
import { api, apiURL, unwrap, type Run, type RunMessage } from "../api/client";
import { useEventStream } from "../api/sse";
import { useAuth } from "../auth/AuthContext";
import { fpath } from "../components/FactoryLayout";
import { confirm } from "../components/overlays";
import { Alert, Button, Empty, ErrorAlert, Keys, KV, PageHeader, Skeleton, StateBadge, StatusIcon, TimeAgo } from "../components/ui";
import { count, duration, firstLine, fmtClock, fmtTime, humanizeError, money, shortID, stateLabel } from "../lib/format";
import { useMutate } from "../lib/mutate";
import { isSubmitChord, MOD } from "../lib/queries";
import { runTree } from "./Work";

const RUN_STATES = ["QUEUED", "PENDING", "CLAIMED", "RUNNING", "BLOCKED", "SUCCEEDED", "FAILED", "ERROR", "CANCELLED"] as const;

export function isTerminal(state?: string): boolean {
  return state === "SUCCEEDED" || state === "FAILED" || state === "ERROR" || state === "CANCELLED";
}

/** principalLabel names the principal that a run acts as. */
export function principalLabel(p: { type: string; id: string; name?: string }): string {
  const name = p.name || p.id;
  switch (p.type) {
    case "service_account":
      return `${name} (service account)`;
    case "factory":
      return `${name} (factory)`;
    default:
      return name;
  }
}

/** Makes a blocked_reason code readable, e.g. human_request becomes "human request". */
export function blockedLabel(reason: string): string {
  return `waiting: ${reason.replace(/_/g, " ")}`;
}

export function RunTable({ runs }: { runs: Run[] }) {
  if (runs.length === 0)
    return (
      <Empty icon={<Activity size={18} />} title="No runs yet">
        A run starts when an agent takes a work item.
      </Empty>
    );
  return (
    <ul className="list bleed" aria-label="Runs">
      {runTree(runs, { newestFirst: true }).map(({ run, depth }) => (
        <li key={run.run_id}>
          <div className="list-row">
            <span style={{ paddingLeft: `${depth * 20}px`, display: "inline-flex" }}>
              <StatusIcon state={run.state} />
            </span>
            <Link to={`/runs/${encodeURIComponent(run.run_id)}`} className="row-title" data-row style={{ color: "inherit" }}>
              {(run.title && run.title !== run.agent_name ? run.title : "") || firstLine(run.prompt) || run.name || shortID(run.run_id)}
            </Link>
            <span className="row-meta">
              {run.state === "BLOCKED" && run.blocked_reason && <span className="state-label">{blockedLabel(run.blocked_reason)}</span>}
              <span className={run.state === "SUCCEEDED" || (run.state === "BLOCKED" && run.blocked_reason) ? "sr-only" : "state-label"}>{stateLabel(run.state)}</span>
              {run.agent_name && <span className="chip hide-sm">{run.agent_name}</span>}
              <span className="hide-sm faint">{run.config?.harness?.model}</span>
              <span className="num" style={{ width: 52 }}>
                {money(run.usage?.cost_usd)}
              </span>
            </span>
            <span className="row-time">
              <TimeAgo t={run.created_at} />
            </span>
          </div>
        </li>
      ))}
    </ul>
  );
}

/** Team runs (/runs) or a factory's runs (/f/:fid/runs). */
export function RunsPage({ factoryID }: { factoryID?: string }) {
  const [params, setParams] = useSearchParams();
  const state = params.get("state") ?? "";
  const agent = params.get("agent") ?? "";
  const q = useInfiniteQuery({
    queryKey: ["runs", { factoryID, state, agent }],
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      unwrap(
        api.GET("/runs", {
          params: { query: { factory_id: factoryID, state: state || undefined, agent_name: agent || undefined, limit: 100, cursor: pageParam || undefined } },
        }),
      ),
    getNextPageParam: (last) => (last.page_info?.has_next_page ? last.page_info.next_cursor : undefined),
  });
  const runs = (q.data?.pages ?? []).flatMap((p) => p.items ?? []);
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };
  return (
    <>
      <PageHeader title="Runs" />
      <div className="toolbar" role="search">
        <label className="sr-only" htmlFor="rstate">
          State
        </label>
        <select id="rstate" value={state} onChange={(e) => set("state", e.target.value)}>
          <option value="">Any state</option>
          {RUN_STATES.map((s) => (
            <option key={s} value={s}>
              {s.charAt(0) + s.slice(1).toLowerCase()}
            </option>
          ))}
        </select>
        <label className="sr-only" htmlFor="ragent">
          Agent
        </label>
        <input
          id="ragent"
          type="search"
          placeholder="Filter by agent…"
          defaultValue={agent}
          onBlur={(e) => set("agent", e.target.value.trim())}
          onKeyDown={(e) => e.key === "Enter" && set("agent", (e.target as HTMLInputElement).value.trim())}
        />
      </div>
      {q.isLoading ? (
        <Skeleton rows={6} />
      ) : q.isError ? (
        <ErrorAlert error={q.error} />
      ) : runs.length === 0 && (state || agent) ? (
        <Empty title="No runs match">Change or clear the filters.</Empty>
      ) : (
        <>
          <RunTable runs={runs} />
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

const ROLE_LABEL: Record<string, string> = {
  user: "User",
  assistant: "Assistant",
  tool_call: "Tool call",
  tool_result: "Tool result",
  system: "System",
  log: "Log",
};

const TOOL_KIND: Record<string, string> = { tool_call: "Tool", tool_result: "Result", log: "Log" };

export function TranscriptMessage({ m }: { m: RunMessage }) {
  const collapsible = m.role === "tool_call" || m.role === "tool_result" || m.role === "log";
  const summary = firstLine(m.content, 160);
  return (
    <li className={`msg msg-${m.role}`} data-seq={m.seq}>
      <div className="msg-meta">
        <span className="msg-role">{ROLE_LABEL[m.role] ?? m.role}</span>
        <time dateTime={m.created_at} title={fmtTime(m.created_at)}>
          {fmtClock(m.created_at)}
        </time>
      </div>
      {collapsible ? (
        <details>
          <summary>
            <ChevronRight size={12} aria-hidden="true" />
            <span className="tool-kind">{TOOL_KIND[m.role]}</span>
            <span className="tool-text">{summary || "(empty)"}</span>
          </summary>
          <pre className="code">{m.data && Object.keys(m.data).length > 0 ? JSON.stringify(m.data, null, 2) : m.content}</pre>
        </details>
      ) : (
        <div className="msg-body">{m.content}</div>
      )}
    </li>
  );
}

/** Merges messages by seq, keeping order and dropping duplicates. */
export function mergeMessages(prev: RunMessage[], incoming: RunMessage[]): RunMessage[] {
  const bySeq = new Map(prev.map((m) => [m.seq, m]));
  for (const m of incoming) bySeq.set(m.seq, m);
  return Array.from(bySeq.values()).sort((a, b) => a.seq - b.seq);
}

/**
 * Live transcript: replays every message then streams new ones and state
 * changes from GET /runs/{id}/events until the run finishes.
 */
export function useRunStream(runID: string, initial?: Run) {
  const qc = useQueryClient();
  const [messages, setMessages] = useState<RunMessage[]>([]);
  const [run, setRun] = useState<Run | undefined>(initial);
  const [done, setDone] = useState(false);
  useEffect(() => {
    setMessages([]);
    setDone(false);
    setRun(undefined);
  }, [runID]);
  useEffect(() => {
    if (initial) setRun((r) => r ?? initial);
  }, [initial]);
  const url = done ? null : apiURL(`/runs/${encodeURIComponent(runID)}/events`);
  const stream = useEventStream(url, {
    run: (d) => setRun(d as Run),
    message: (d) => setMessages((prev) => mergeMessages(prev, [d as RunMessage])),
    done: (d) => {
      setRun(d as Run);
      setDone(true);
      void qc.invalidateQueries({ queryKey: ["run", runID] });
    },
  });
  return { messages, run, live: stream.status === "open" && !done, status: stream.status, done };
}

export function RunPage() {
  const { id = "" } = useParams();
  const { canEditFactory } = useAuth();
  const q = useQuery({ queryKey: ["run", id], queryFn: () => unwrap(api.GET("/runs/{runId}", { params: { path: { runId: id } } })) });
  const stream = useRunStream(id, q.data);
  const run = stream.run?.run_id === id ? stream.run : q.data;
  const children = useQuery({
    queryKey: ["runs", { parent: id }],
    queryFn: () => unwrap(api.GET("/runs", { params: { query: { parent_run_id: id, limit: 200 } } })).then((r) => r.items ?? []),
  });
  const scorers = useQuery({
    queryKey: ["scorers", run?.factory_id],
    enabled: !!run?.factory_id,
    queryFn: () => unwrap(api.GET("/factories/{uid}/scorers", { params: { path: { uid: run!.factory_id! } } })).then((r) => r.items ?? []),
  });
  const [followUp, setFollowUp] = useState("");
  const [scorerID, setScorerID] = useState("");
  const send = useMutate(
    (message: string) => unwrap(api.POST("/runs/{runId}/followups", { params: { path: { runId: id } }, body: { message } })),
    { success: "Follow-up sent.", onSuccess: () => setFollowUp("") },
  );
  const cancel = useMutate(() => unwrap(api.POST("/runs/{runId}/cancel", { params: { path: { runId: id } } })), {
    invalidate: [["run", id]],
    success: "Run stopped.",
  });
  const score = useMutate(
    (sid: string) => unwrap(api.POST("/runs/{runId}/score", { params: { path: { runId: id } }, body: { scorer_id: sid } })),
    { success: (s) => `Scored: ${s.label ?? ""}${s.passed ? " (passed)" : " (failed)"}` },
  );
  const transcriptEnd = useRef<HTMLLIElement>(null);
  const messageCount = stream.messages.length;
  useEffect(() => {
    if (stream.live) transcriptEnd.current?.scrollIntoView?.({ block: "nearest" });
  }, [messageCount, stream.live]);
  const parent = useQuery({
    queryKey: ["run", run?.parent_run_id],
    enabled: !!run?.parent_run_id,
    queryFn: () => unwrap(api.GET("/runs/{runId}", { params: { path: { runId: run!.parent_run_id! } } })),
  });
  const crumbs = useMemo(() => {
    const c = [{ label: "Runs", to: run?.factory_id ? fpath(run.factory_id, "runs") : "/runs" }];
    if (parent.data) c.push({ label: parent.data.name || shortID(parent.data.run_id), to: `/runs/${parent.data.run_id}` });
    return c;
  }, [run?.factory_id, parent.data]);

  if (q.isLoading) return <Skeleton rows={6} />;
  if (q.isError || !run) return <ErrorAlert error={q.error ?? new Error("Run not found.")} />;
  const terminal = isTerminal(run.state);
  const failure = run.state !== "BLOCKED" ? humanizeError(run.status_message?.message || run.error) : { message: "", detail: "" };
  const submitFollowUp = () => {
    if (followUp.trim() && !send.isPending) send.mutate(followUp.trim());
  };

  return (
    <>
      <PageHeader
        display
        title={run.name || run.agent_name || run.title || run.run_id}
        crumbs={crumbs}
        subtitle={
          <>
            <StateBadge state={run.state} />
            {run.state === "BLOCKED" && run.blocked_reason && <span>{blockedLabel(run.blocked_reason)}</span>}
            {run.agent_name && (
              <span className="row">
                <Bot size={13} aria-hidden="true" /> {run.agent_name}
              </span>
            )}
            <span>
              {run.config?.harness?.type}
              {run.config?.harness?.model ? ` / ${run.config.harness.model}` : ""}
            </span>
            <TimeAgo t={run.created_at} />
            {run.work_item_id && run.factory_id && <Link to={fpath(run.factory_id, `work/${run.work_item_id}`)}>Work item</Link>}
          </>
        }
        actions={
          !terminal && (
            <Button
              variant="danger"
              onClick={async () => {
                if (await confirm({ title: "Stop this run?", body: "The agent stops at once. Its child runs stop too.", confirmLabel: "Stop run", cancelLabel: "Keep running", danger: true }))
                  cancel.mutate(undefined);
              }}
              busy={cancel.isPending}
            >
              <Square size={12} aria-hidden="true" /> Stop run
            </Button>
          )
        }
      />
      <div className="two-col">
        <div className="stack">
          {run.state === "BLOCKED" && (
            <Alert kind="warning">
              <strong>Waiting for a person.</strong> {run.blocked_reason ? blockedLabel(run.blocked_reason) : ""}
              {run.status_message?.message ? ` ${run.status_message.message}` : ""}
            </Alert>
          )}
          {failure.message && (
            <div className="error-panel" role="alert">
              <div className="error-title">
                <StatusIcon state="failed" />
                {run.state === "ERROR" ? "Run error" : "Run failed"}
              </div>
              <div style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>{failure.message}</div>
              {(run.status_message?.failure_code || run.status_message?.detail) && (
                <div className="muted small">
                  {run.status_message?.failure_code && <code>{run.status_message.failure_code}</code>}
                  {run.status_message?.retryable ? " · retryable" : ""}
                  {run.status_message?.detail ? ` · ${run.status_message.detail}` : ""}
                </div>
              )}
              {failure.detail && (
                <details>
                  <summary>Technical details</summary>
                  <pre className="code">{failure.detail}</pre>
                </details>
              )}
            </div>
          )}
          {run.result && (
            <section aria-label="Result" className="stack stack-sm">
              <h2 className="section-title">Result</h2>
              <div className="tl-text" style={{ marginTop: 0 }}>
                {run.result}
              </div>
            </section>
          )}
          <section className="stack stack-sm" aria-labelledby="transcript-title">
            <div className="section-head" style={{ marginBottom: 0 }}>
              <h2 className="section-title" id="transcript-title">
                Transcript
              </h2>
              {stream.live ? (
                <span className="small live-indicator" aria-live="polite">
                  ● Live
                </span>
              ) : undefined}
            </div>
            {stream.messages.length === 0 ? (
              <p className="muted small">{stream.status === "connecting" ? "Connecting…" : "No messages yet. New messages stream in automatically."}</p>
            ) : (
              <ol className="transcript" aria-live="polite" aria-label="Transcript">
                {stream.messages.map((m) => (
                  <TranscriptMessage key={m.seq} m={m} />
                ))}
                <li ref={transcriptEnd} aria-hidden="true" className="transcript-end" />
              </ol>
            )}
          </section>
          {!terminal && (
            <form
              className="reply"
              aria-label="Follow up"
              onSubmit={(e) => {
                e.preventDefault();
                submitFollowUp();
              }}
            >
              <label className="sr-only" htmlFor="fu">
                Follow-up message
              </label>
              <textarea
                id="fu"
                rows={3}
                required
                value={followUp}
                onChange={(e) => setFollowUp(e.target.value)}
                onKeyDown={(e) => isSubmitChord(e) && submitFollowUp()}
                placeholder="Send the agent further instructions…"
              />
              <div className="reply-foot">
                <Button type="submit" variant="primary" size="sm" busy={send.isPending} disabled={!followUp.trim()}>
                  Send follow-up <Keys k={`${MOD} ↵`} />
                </Button>
              </div>
            </form>
          )}
        </div>
        <aside className="props" aria-label="Run details">
          <section className="props-section">
            <h2>Usage</h2>
            <KV
              items={[
                ["Cost", <span key="c">{money(run.usage?.cost_usd)}</span>],
                ["Tokens in", <span key="i">{count(run.usage?.input_tokens)}</span>],
                ["Tokens out", <span key="o">{count(run.usage?.output_tokens)}</span>],
                ["Duration", <span key="d">{duration(run.usage?.duration_ms)}</span>],
              ]}
            />
          </section>
          <section className="props-section">
            <h2>Details</h2>
            <KV
              items={[
                ["Run ID", <code key="id">{run.run_id}</code>],
                ["Source", run.source_ref ? `${run.source} (${run.source_ref})` : run.source],
                ...(run.creator ? ([["Creator", run.creator.name || run.creator.id]] as [string, string][]) : []),
                ...(run.run_as ? ([["Runs as", principalLabel(run.run_as)]] as [string, string][]) : []),
                ...(run.config?.machine?.name ? ([["Machine", run.config.machine.name]] as [string, string][]) : []),
                ...(run.worker_id ? ([["Worker", run.worker_id]] as [string, string][]) : []),
                ["Started", fmtTime(run.started_at)],
                ["Finished", fmtTime(run.finished_at)],
              ]}
            />
            {run.metadata && Object.keys(run.metadata).length > 0 && (
              <>
                <h2 style={{ marginTop: "var(--s3)" }}>Metadata</h2>
                <KV items={Object.entries(run.metadata)} />
              </>
            )}
          </section>
          <section className="props-section">
            <h2>Sub-agents</h2>
            <ul className="run-tree">
              {(children.data ?? []).map((c) => (
                <li key={c.run_id}>
                  <StatusIcon state={c.state} />
                  <Link to={`/runs/${encodeURIComponent(c.run_id)}`}>{c.name || c.agent_name || shortID(c.run_id)}</Link>
                  <span className="cost">{money(c.usage?.cost_usd)}</span>
                </li>
              ))}
              {(children.data ?? []).length === 0 && <li className="muted small">No child runs.</li>}
            </ul>
          </section>
          <section className="props-section">
            <h2>Prompt</h2>
            <div className="prose small">{run.prompt}</div>
          </section>
          {!!run.factory_id && canEditFactory(run.factory_id) && (scorers.data ?? []).length > 0 && (
            <section className="props-section">
              <h2>Score this run</h2>
              <form
                className="input-row"
                onSubmit={(e) => {
                  e.preventDefault();
                  score.mutate(scorerID || scorers.data![0].id!);
                }}
              >
                <label className="sr-only" htmlFor="scorer">
                  Scorer
                </label>
                <select id="scorer" value={scorerID} onChange={(e) => setScorerID(e.target.value)}>
                  {scorers.data!.map((s) => (
                    <option key={s.id} value={s.id}>
                      {s.name || s.slug}
                    </option>
                  ))}
                </select>
                <Button type="submit" busy={score.isPending}>
                  Score
                </Button>
              </form>
            </section>
          )}
        </aside>
      </div>
    </>
  );
}
