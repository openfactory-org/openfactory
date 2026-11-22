// Factory MCP tools over work items: find, read, start working locally,
// converse with the foreman, send or hand back, complete.
import { OfError, runLink, splitRepo, STAGES } from "@openfactory/core";
import type { Principal } from "../../../contracts/do-messages";
import {
  firstLine,
  matchesWorkItem,
  parsePullRequestURL,
  planReference,
  repoSetupFor,
  shortId,
  slug,
  truncate,
  workRefs,
  type PullRequestRef,
  type RepoSetup,
} from "../refs";
import {
  actorOf,
  b,
  bool,
  factoryOf,
  int,
  invalid,
  listFactoriesAll,
  listWorkItemsOf,
  n,
  notFound,
  obj,
  s,
  str,
  TERMINAL_STAGES,
  tool,
  userIdOf,
  workItemDetail,
  workItemURL,
  workItemView,
  type Args,
  type FactoryCtx,
  type Json,
  type RunJson,
  type WorkItem,
} from "./common";
import { checkRoute } from "./notifications";

const TERMINAL_RUN_STATES = new Set(["completed", "failed", "cancelled", "succeeded", "error"]);

/** InboxForCaller: the items that need the caller. */
function forCaller(it: { recipients?: unknown[] }, p: Principal): boolean {
  const rc = (it.recipients ?? [])
    .map((r) => (typeof r === "string" ? r : (((r as Json)?.user_id ?? (r as Json)?.id) as string | undefined)))
    .filter((x): x is string => !!x);
  if (rc.length === 0) return true;
  return userIdOf(p) !== "" && rc.includes(userIdOf(p));
}

function parseTime(v: string): string {
  if (/^\d{4}-\d{2}-\d{2}$/.test(v) && !Number.isNaN(Date.parse(v + "T00:00:00Z"))) return v + "T00:00:00Z";
  if (/^\d{4}-\d{2}-\d{2}T/.test(v) && !Number.isNaN(Date.parse(v))) return v;
  throw invalid(`invalid time "${v}": use RFC 3339 or YYYY-MM-DD`);
}

/** resolveWorkItem: a work item ID or a reference (URL, issue, pull request, branch). */
async function resolveWorkItem(c: FactoryCtx, ref0: string): Promise<WorkItem> {
  const ref = ref0.trim();
  if (ref === "") throw invalid("work item is required");
  const nf = notFound(`no work item matches "${ref}"`);
  const owned = async (id: string): Promise<WorkItem | null> => {
    try {
      return await workItemDetail(c, id);
    } catch {
      return null;
    }
  };
  const ofRun = async (runId: string): Promise<WorkItem | null> => {
    try {
      const r = await c.backend.api<RunJson>(c.env, c.principal, "GET", `/runs/${encodeURIComponent(runId)}`);
      return r.work_item_id ? await owned(r.work_item_id) : null;
    } catch {
      return null;
    }
  };
  if (/^[A-Za-z0-9_.-]+$/.test(ref)) {
    const wi = (await owned(ref)) ?? (await ofRun(ref));
    if (wi) return wi;
  }
  const plan = planReference(ref, c.publicURL);
  if (plan.kind === "notFound") throw nf;
  if (plan.kind === "dashboard") {
    for (const id of plan.workItems) {
      const wi = await owned(id);
      if (wi) return wi;
    }
    for (const id of plan.runs) {
      const wi = await ofRun(id);
      if (wi) return wi;
    }
    throw nf;
  }
  const fs = await listFactoriesAll(c);
  // Fast path: active work item by source key.
  for (const key of plan.keys) {
    for (const f of fs) {
      const id = await c.backend.resolveSourceKey(c.env, c.principal, f.id, key).catch(() => null);
      if (id) {
        const wi = await owned(id);
        if (wi) return wi;
      }
    }
  }
  // Slow path: scan recent work items (terminal ones too), matching source
  // keys, linked pull requests and branches.
  let best: WorkItem | null = null;
  for (const f of fs) {
    for (const wi of await listWorkItemsOf(c, f.id, 500)) {
      if (
        matchesWorkItem(wi, plan.keys, plan.prURL, plan.branch) &&
        (!best || Date.parse(wi.updated_at) > Date.parse(best.updated_at))
      ) {
        best = wi;
      }
    }
  }
  if (!best) throw nf;
  return best;
}

async function factoryRepos(
  c: FactoryCtx,
  factoryId: string,
): Promise<{ codeForge: string; owner: string; name: string }[]> {
  try {
    const d = await c.backend.api<Json>(
      c.env,
      c.principal,
      "GET",
      `/factories/${encodeURIComponent(factoryId)}/definition`,
    );
    const compiled = (d?.compiled ?? d?.definition ?? d) as Json;
    const repos = (compiled?.repositories ?? []) as { codeForge?: string; owner?: string; name?: string }[];
    return repos.map((r) => ({ codeForge: r.codeForge ?? "GITHUB", owner: r.owner ?? "", name: r.name ?? "" }));
  } catch {
    return [];
  }
}

/** startWorking: marks the work item as taken over locally and returns Git setup guidance. */
async function startWorking(c: FactoryCtx, wi: WorkItem, activeRuns: number): Promise<Json> {
  const p = c.principal;
  await c.backend.patchWorkItem(c.env, p, wi.id, {
    local_owner: userIdOf(p),
    event: { kind: "note", actor: actorOf(p), data: { local_owner: userIdOf(p), picked_up_locally: true } },
  });
  wi.local_owner = userIdOf(p);
  wi.updated_at = new Date().toISOString();
  const sid = shortId(wi.id);
  let newBranch = "factory/" + sid;
  const sl = slug(wi.title);
  if (sl) newBranch += "-" + sl;
  const setups: RepoSetup[] = [];
  const seen = new Set<string>();
  for (const pr of wi.pull_requests ?? []) {
    if (!pr.branch || seen.has(pr.repo) || pr.state === "merged" || pr.state === "closed") continue;
    seen.add(pr.repo);
    const [owner, name] = splitRepo(pr.repo);
    setups.push(repoSetupFor(pr.forge, owner, name, pr.branch, "", sid, true));
  }
  if (setups.length === 0) {
    let repos = await factoryRepos(c, wi.factory_id);
    const srcRepo = wi.source?.repo ?? "";
    if (srcRepo) {
      const [owner, name] = splitRepo(srcRepo);
      repos = [{ codeForge: wi.source?.provider === "gitlab" ? "GITLAB" : "GITHUB", owner, name }, ...repos];
    }
    for (const r of repos) {
      const full = `${r.owner}/${r.name}`;
      if (seen.has(full)) continue;
      seen.add(full);
      setups.push(repoSetupFor(r.codeForge, r.owner, r.name, newBranch, "main", sid, false));
    }
  }
  const warnings = [
    "Picking up a work item does not claim, lock, or pause it: the factory may keep running its own work on it.",
  ];
  if (activeRuns > 0) {
    warnings.push(
      `${activeRuns} run(s) are still active on this work item; coordinate with the foreman to avoid duplicate changes.`,
    );
  }
  if (setups.length === 0) {
    warnings.push("No repository is linked to this work item or its factory; ask the foreman which repository to use.");
  }
  return {
    local_owner: userIdOf(p),
    next_steps: [
      "Run the setup commands yourself; Factory MCP never modifies your files.",
      "Tell the foreman you are picking the work item up with message_foreman, and read replies with get_conversation.",
      "Validate your change, commit and push the branch: the factory cannot see uncommitted or unpushed work.",
      "Hand back with send_work_item using work_item_id, the pushed branch or pull_request_url, and a note covering what changed, what was validated, and what remains.",
      "When nothing remains for the factory to do, call complete_work_item.",
    ],
    repositories: setups,
    warnings,
  };
}

async function handBack(c: FactoryCtx, a: Args, pr: PullRequestRef | null): Promise<Json> {
  const p = c.principal;
  const wi = await workItemDetail(c, s(a, "work_item_id"));
  if (TERMINAL_STAGES.has(wi.stage)) throw new OfError("conflict", `work item ${wi.id} is already ${wi.stage}`);
  const f = await factoryOf(c, wi.factory_id);
  if (s(a, "notify_route")) await checkRoute(c, f, s(a, "notify_route"));
  if (pr && !(wi.pull_requests ?? []).some((x) => x.url === pr.url))
    await c.backend.linkPullRequest(c.env, p, wi.id, pr);
  const who = userIdOf(p);
  let msg = `## Work handed back from a local session (${who})\n\n${s(a, "note")}`;
  const refs = workRefs(s(a, "branch"), s(a, "pull_request_url"));
  if (refs) msg += "\n\n" + refs;
  msg += "\n\nDecide the next step for this work item.";
  let runId = wi.root_run_id ?? "";
  if (runId) {
    // The followups route applies restrictFollowupsToStarter, as message_foreman.
    await c.backend.api(c.env, p, "POST", `/runs/${encodeURIComponent(runId)}/followups`, { message: msg });
  } else {
    const run = await c.backend.api<RunJson>(c.env, p, "POST", `/factories/${encodeURIComponent(wi.factory_id)}/runs`, {
      prompt: msg,
      title: wi.title,
      source: wi.source ?? {},
    });
    runId = run.run_id ?? run.id ?? "";
  }
  // Clear the local owner and record the hand-back on the timeline.
  await c.backend.patchWorkItem(c.env, p, wi.id, {
    local_owner: "",
    ...(s(a, "notify_route") ? { labels: { notify_route: s(a, "notify_route") } } : {}),
    event: {
      kind: "note",
      actor: actorOf(p),
      data: { handed_back: true, branch: s(a, "branch"), pull_request_url: s(a, "pull_request_url") },
    },
  });
  return {
    note: "The work returned to the same work item; the foreman decides the next step. Handing back does not complete the work item.",
    run_id: runId,
    status: "handed_back",
    url: workItemURL(c, wi.factory_id, wi.id),
    work_item_id: wi.id,
  };
}

export const listWorkItems = tool(
  "list_work_items",
  "Lists the work items in one factory, with filters such as creator, stage, and date.",
  true,
  obj(
    {
      factory_id: str("factory UID"),
      creator: str("'me' or a user ID"),
      stage: str(
        "comma-separated stages: intake, triage, planning, awaiting_approval, building, reviewing, handoff, complete, cancelled",
      ),
      since: str("RFC 3339 time or YYYY-MM-DD"),
      limit: int("page size (default 20, max 100)"),
      cursor: str("next_cursor from a previous call"),
    },
    ["factory_id"],
  ),
  async (c, a) => {
    const f = await factoryOf(c, s(a, "factory_id"));
    let limit = n(a, "limit");
    if (limit <= 0) limit = 20;
    if (limit > 100) limit = 100;
    const q = new URLSearchParams({ limit: String(limit) });
    if (s(a, "cursor")) q.set("cursor", s(a, "cursor"));
    const creator = s(a, "creator");
    if (creator === "me") q.set("creator", userIdOf(c.principal));
    else if (creator !== "") q.set("creator", creator);
    if (s(a, "stage") !== "") {
      const stages = s(a, "stage")
        .split(",")
        .map((x) => x.trim().toLowerCase());
      for (const st of stages) if (!(STAGES as readonly string[]).includes(st)) throw invalid(`unknown stage "${st}"`);
      q.set("stage", stages.join(","));
    }
    if (s(a, "since") !== "") q.set("since", parseTime(s(a, "since")));
    const page = await c.backend.api<{ items: WorkItem[]; page_info?: { next_cursor?: string } }>(
      c.env,
      c.principal,
      "GET",
      `/factories/${encodeURIComponent(f.id)}/work-items?${q}`,
    );
    const out: Json = {};
    if (page.page_info?.next_cursor) out.next_cursor = page.page_info.next_cursor;
    out.work_items = (page.items ?? []).map((w) => workItemView(c, w));
    return out;
  },
);

export const searchWorkItems = tool(
  "search_work_items",
  "Searches work item titles across all factories you can access.",
  true,
  obj({ query: str("text to search for in work item titles"), limit: int("maximum results (default 20)") }, ["query"]),
  async (c, a) => {
    const q = s(a, "query").trim();
    if (q === "") throw invalid("query is required");
    let limit = n(a, "limit");
    if (limit <= 0 || limit > 100) limit = 20;
    const all: WorkItem[] = [];
    for (const f of await listFactoriesAll(c)) {
      const items = await listWorkItemsOf(c, f.id, limit, { search: q });
      for (const it of items) if (it.title.toLowerCase().includes(q.toLowerCase())) all.push(it);
    }
    all.sort((x, y) => Date.parse(y.updated_at) - Date.parse(x.updated_at));
    return { work_items: all.slice(0, limit).map((w) => workItemView(c, w)) };
  },
);

export const getWorkItem = tool(
  "get_work_item",
  "Reads a work item's status, run history, and outputs. Accepts a work item ID or a reference such as a URL, issue, pull request, or branch. With start_working=true, also returns local setup guidance.",
  false,
  obj(
    {
      work_item: str(
        "a work item ID or a reference: work item or run URL, pull request or issue URL, owner/repo#123, Slack permalink, Linear or Jira issue key or URL, or a branch name",
      ),
      start_working: bool("also return local setup guidance for continuing the work item in a local checkout"),
    },
    ["work_item"],
  ),
  async (c, a) => {
    const wi = await resolveWorkItem(c, s(a, "work_item"));
    const d = await workItemDetail(c, wi.id);
    let active = 0;
    let output = "";
    const runs = (d.runs ?? []).map((r) => {
      if (!TERMINAL_RUN_STATES.has(r.state)) active++;
      const id = r.run_id ?? r.id ?? "";
      if (id === d.root_run_id) output = r.result ?? "";
      return {
        run_id: id,
        ...(r.agent_name ? { agent_name: r.agent_name } : {}),
        ...(r.name ? { name: r.name } : {}),
        state: r.state,
        ...(r.parent_run_id ? { parent_run_id: r.parent_run_id } : {}),
        ...(r.result ? { result: truncate(r.result, 2000) } : {}),
        ...(r.error ? { error: r.error } : {}),
        url: r.run_url ?? r.url ?? runLink(c.publicURL, id),
        created_at: r.created_at,
        ...(r.finished_at ? { finished_at: r.finished_at } : {}),
      };
    });
    const pending = (d.inbox ?? [])
      .filter((it) => !it.resolved && forCaller(it, c.principal))
      .map((it) => ({
        id: it.id,
        kind: it.kind,
        options: it.options ?? null,
        ...(it.questions && it.questions.length > 0 ? { questions: it.questions } : {}),
        title: it.title,
      }));
    const out: Json = {
      active_runs: active,
      needs_decision: pending,
      output: truncate(output, 8000),
      runs,
      work_item: workItemView(c, d),
    };
    if (b(a, "start_working")) {
      out.local_setup = await startWorking(c, d, active);
      out.work_item = workItemView(c, d);
    }
    return out;
  },
);

export const messageForeman = tool(
  "message_foreman",
  "Sends a message to a work item's foreman.",
  false,
  obj({ work_item_id: str("work item ID"), message: str("progress, a question, or a blocker for the foreman") }, [
    "work_item_id",
    "message",
  ]),
  async (c, a) => {
    const res = await c.backend.api<{ work_item_id?: string; run_id?: string }>(
      c.env,
      c.principal,
      "POST",
      `/work-items/${encodeURIComponent(s(a, "work_item_id"))}/messages`,
      { message: s(a, "message") },
    );
    return {
      note: "Messaging keeps the factory informed but does not move or hand back the work item. Read replies with get_conversation.",
      run_id: res.run_id ?? "",
      status: "sent",
      work_item_id: res.work_item_id ?? s(a, "work_item_id"),
    };
  },
);

export const getConversation = tool(
  "get_conversation",
  "Reads a work item's foreman conversation.",
  true,
  obj(
    {
      work_item_id: str("work item ID"),
      after: int("only return messages with a sequence number greater than this"),
    },
    ["work_item_id"],
  ),
  async (c, a) => {
    const d = await workItemDetail(c, s(a, "work_item_id"));
    const out: Json = { messages: [], stage: d.stage, work_item_id: d.id };
    if (!d.root_run_id) return out;
    const q = new URLSearchParams({ after: String(n(a, "after")) });
    const msgs = await c.backend.api<{
      messages?: { seq: number; role: string; content: string; created_at: string }[];
    }>(c.env, c.principal, "GET", `/runs/${encodeURIComponent(d.root_run_id)}/messages?${q}`);
    let last = 0;
    const conv: Json[] = [];
    for (const m of msgs.messages ?? []) {
      last = m.seq;
      if (m.role !== "user" && m.role !== "assistant") continue;
      conv.push({ seq: m.seq, role: m.role, content: m.content, created_at: m.created_at });
    }
    out.run_id = d.root_run_id;
    out.messages = conv;
    if (last > 0) out.last_seq = last;
    for (const r of d.runs ?? []) if ((r.run_id ?? r.id) === d.root_run_id) out.foreman_state = r.state;
    return out;
  },
);

export const sendWorkItem = tool(
  "send_work_item",
  "Creates a new work item, or hands work back to an existing one.",
  false,
  obj(
    {
      factory_id: str("target factory UID (required for a new work item)"),
      work_item_id: str("existing work item to hand work back to"),
      title: str("title of a new work item"),
      note: str(
        "what the foreman starts from: goal, context, constraints and work already done; when handing back: what changed, what was validated, what remains",
      ),
      branch: str("pushed branch holding the work"),
      pull_request_url: str("pull request holding the work"),
      notify_route: str(
        "route ID from list_notification_routes to be notified when the work item needs you or finishes",
      ),
    },
    ["note"],
  ),
  async (c, a) => {
    if (s(a, "note").trim() === "") throw invalid("note is required");
    let pr: PullRequestRef | null = null;
    if (s(a, "pull_request_url") !== "") {
      pr = parsePullRequestURL(s(a, "pull_request_url"));
      if (!pr) throw invalid("pull_request_url is not a recognized pull or merge request URL");
      if (s(a, "branch")) pr.branch = s(a, "branch");
    }
    if (s(a, "work_item_id") !== "") return handBack(c, a, pr);
    if (s(a, "factory_id") === "")
      throw invalid("factory_id is required for a new work item (or pass work_item_id to hand work back)");
    const f = await factoryOf(c, s(a, "factory_id"));
    if (s(a, "notify_route")) await checkRoute(c, f, s(a, "notify_route"));
    const title = s(a, "title").trim() || firstLine(s(a, "note"));
    let prompt = s(a, "note");
    const refs = workRefs(s(a, "branch"), s(a, "pull_request_url"));
    if (refs) prompt += "\n\n" + refs;
    const run = await c.backend.api<RunJson & { work_item_id: string }>(
      c.env,
      c.principal,
      "POST",
      `/factories/${encodeURIComponent(f.id)}/runs`,
      {
        prompt,
        title,
        // The route records triggered_by mcp for a Factory MCP principal (requestSource).
        source: { provider: "mcp", kind: "work_item", title },
      },
    );
    const wiId = run.work_item_id;
    const runId = run.run_id ?? run.id ?? "";
    if (pr) await c.backend.linkPullRequest(c.env, c.principal, wiId, pr);
    if (s(a, "notify_route"))
      await c.backend.patchWorkItem(c.env, c.principal, wiId, { labels: { notify_route: s(a, "notify_route") } });
    return {
      note: "The foreman takes it from here and reports progress on the work item's conversation.",
      run_id: runId,
      run_url: runLink(c.publicURL, runId),
      status: "created",
      url: workItemURL(c, f.id, wiId),
      work_item_id: wiId,
    };
  },
);

export const completeWorkItem = tool(
  "complete_work_item",
  "Marks a work item complete.",
  false,
  obj({ work_item_id: str("work item ID"), note: str("closing note") }, ["work_item_id"]),
  async (c, a) => {
    const wi = await c.backend.api<WorkItem>(
      c.env,
      c.principal,
      "POST",
      `/work-items/${encodeURIComponent(s(a, "work_item_id"))}/complete`,
      {
        ...(s(a, "note") ? { note: s(a, "note") } : {}),
      },
    );
    return { status: "completed", work_item: workItemView(c, wi) };
  },
);
