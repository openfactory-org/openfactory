import { useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, ApiError, unwrap, type Schemas } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { Plus } from "lucide-react";
import { ComposerForm } from "../components/Composer";
import { FACTORY_ICON, factoryColor } from "../components/nav";
import { Alert, Button, Card, Empty, Field, PageHeader, Query, TimeAgo, ValidationList } from "../components/ui";
import { coverageGaps, describeGaps } from "../lib/coverage";
import { useMutate } from "../lib/mutate";
import { FACTORIES_KEY, fpath, useDispatch, useFactories } from "../lib/queries";

export const AGENT_CATALOG = [
  { name: "triage", label: "Triage", description: "Researches incoming requests, gathers evidence and decides what happens next." },
  { name: "spec", label: "Spec", description: "Turns triaged work into a reviewed technical spec before implementation." },
  { name: "implement", label: "Implement", description: "Implements approved work, runs validation and opens a pull request." },
  { name: "review", label: "Review", description: "Independently reviews pull requests produced by the factory." },
] as const;

export const INTEGRATION_TYPES = [
  { type: "slack", label: "Slack", hint: "Mention the foreman in channels and threads." },
  { type: "microsoft-teams", label: "Microsoft Teams", hint: "Mention the foreman in Teams channels." },
  { type: "linear", label: "Linear", hint: "Assign or mention the foreman on Linear issues." },
  { type: "jira", label: "Jira", hint: "Route Jira issues to the factory." },
] as const;

type RepoRef = Schemas["RepoRef"];

/** Parses "owner/name" lines (optionally "GITLAB:owner/name" or GitHub URLs). */
export function parseRepoLines(src: string): RepoRef[] {
  const out: RepoRef[] = [];
  for (const raw of src.split(/[\n,]/)) {
    let line = raw.trim();
    if (!line) continue;
    let forge = "GITHUB";
    const colon = line.indexOf(":");
    if (colon > 0 && !line.slice(0, colon).includes("/") && !line.startsWith("https:")) {
      forge = line.slice(0, colon).trim().toUpperCase();
      line = line.slice(colon + 1).trim();
    }
    if (!["GITHUB", "GITLAB", "AZURE_DEVOPS"].includes(forge)) throw new Error(`Unknown code forge "${forge}" (use GITHUB, GITLAB or AZURE_DEVOPS).`);
    line = line.replace(/^https:\/\//, "").replace(/^github\.com\//, "").replace(/\.git$/, "");
    const i = line.lastIndexOf("/");
    if (i <= 0 || i === line.length - 1) throw new Error(`Repository "${line}" must look like owner/name.`);
    out.push({ codeForge: forge as RepoRef["codeForge"], owner: line.slice(0, i), name: line.slice(i + 1) });
  }
  return out;
}

export function FactoriesPage() {
  const q = useFactories();
  const { canEdit } = useAuth();
  const [search, setSearch] = useState("");
  return (
    <>
      <PageHeader
        title="Factories"
        actions={
          canEdit && (
            <Link to="/factories/new" className="btn btn-primary">
              <Plus size={14} aria-hidden="true" /> New factory
            </Link>
          )
        }
      />
      <div className="toolbar">
        <label className="sr-only" htmlFor="fsearch">
          Search factories
        </label>
        <input id="fsearch" type="search" placeholder="Search factories…" value={search} onChange={(e) => setSearch(e.target.value)} />
      </div>
      <Query q={q}>
        {(fs) => {
          const rows = fs.filter((f) => `${f.name} ${f.alias} ${f.description ?? ""}`.toLowerCase().includes(search.toLowerCase()));
          if (rows.length === 0)
            return fs.length === 0 ? (
              <Empty
                icon={<FACTORY_ICON size={18} />}
                title="No factories yet"
                action={
                  canEdit && (
                    <Link to="/factories/new" className="btn btn-primary">
                      Create your first factory
                    </Link>
                  )
                }
              >
                A factory has a foreman, a team of agents and the automations that send them work.
              </Empty>
            ) : (
              <Empty title="No factories match">Try a different search.</Empty>
            );
          return (
            <ul className="list bleed" aria-label="Factories">
              {rows.map((f) => (
                <li key={f.id}>
                  <Link to={fpath(f, "work")} className="list-row">
                    <span className="factory-dot" style={{ background: factoryColor(f.id) }} aria-hidden="true">
                      {f.name.slice(0, 1)}
                    </span>
                    <span className="row-title">
                      {f.name}
                      {f.description && <span className="muted" style={{ fontWeight: 400 }}> · {f.description}</span>}
                    </span>
                    <span className="row-meta">
                      <span>@{f.alias}</span>
                      <span className="hide-sm">{f.source === "git" ? `git: ${f.git_repo}` : "managed"}</span>
                    </span>
                    <span className="row-time">
                      <TimeAgo t={f.updated_at} />
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          );
        }}
      </Query>
    </>
  );
}

export function NewFactoryPage() {
  const navigate = useNavigate();
  const [form, setForm] = useState({ name: "", alias: "", description: "", repos: "" });
  const [agents, setAgents] = useState<string[]>(AGENT_CATALOG.map((a) => a.name));
  const [integrations, setIntegrations] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [verrs, setVerrs] = useState<Schemas["ValidationError"][]>([]);
  const teamIntegrations = useQuery({
    queryKey: ["integrations"],
    queryFn: () => unwrap(api.GET("/integrations")).then((r) => r.items ?? []),
    retry: false,
  });
  // A new factory has no factory records yet: only team records can serve its
  // repositories. The warning does not block creation.
  let gapLines: string[] = [];
  if (teamIntegrations.data) {
    try {
      gapLines = describeGaps(coverageGaps(parseRepoLines(form.repos), teamIntegrations.data, ""));
    } catch {
      // The submit reports a malformed line.
    }
  }

  const create = useMutate((body: Schemas["CreateFactoryRequest"]) => unwrap(api.POST("/factories", { body })), {
    invalidate: [FACTORIES_KEY],
    quietErrors: true,
    success: (r) => `Factory ${r.factory.name} created.`,
    onSuccess: (r) => navigate(`${fpath(r.factory.uid)}?welcome=1`),
  });

  function toggle(list: string[], v: string, set: (l: string[]) => void) {
    set(list.includes(v) ? list.filter((x) => x !== v) : [...list, v]);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setVerrs([]);
    if (!form.name.trim()) return setError("Enter a factory name.");
    if (integrations.includes("linear") && integrations.includes("jira")) return setError("Choose at most one issue tracker: Linear and Jira are mutually exclusive.");
    let repositories: RepoRef[];
    try {
      repositories = parseRepoLines(form.repos);
    } catch (err) {
      return setError((err as Error).message);
    }
    try {
      await create.mutateAsync({
        name: form.name.trim(),
        alias: form.alias.trim() || undefined,
        description: form.description.trim() || undefined,
        repositories,
        agents: agents as Schemas["CreateFactoryRequest"]["agents"],
        integrations: integrations.map((type) => ({ type })) as Schemas["IntegrationDecl"][],
      });
    } catch (err) {
      setError((err as Error).message);
      if (err instanceof ApiError) setVerrs(err.validationErrors);
    }
  }

  return (
    <>
      <PageHeader title="New factory" crumbs={[{ label: "Factories", to: "/factories" }]} />
      <form className="stack narrow" onSubmit={submit} noValidate>
        {error && <Alert kind="error">{error}</Alert>}
        <ValidationList errors={verrs} />
        <Card title="Identity">
          <Field label="Name" htmlFor="name">
            <input id="name" required maxLength={100} value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </Field>
          <Field label="Foreman name" htmlFor="alias" hint="The handle your team @-mentions. Defaults to the factory name.">
            <input id="alias" maxLength={60} value={form.alias} onChange={(e) => setForm({ ...form, alias: e.target.value })} />
          </Field>
          <Field label="Description" htmlFor="description">
            <textarea id="description" rows={2} value={form.description} onChange={(e) => setForm({ ...form, description: e.target.value })} />
          </Field>
        </Card>
        <Card title="Repositories">
          <Field label="Repositories" htmlFor="repos" hint="One per line: owner/name, or GITLAB:group/name, AZURE_DEVOPS:org/project/repo.">
            <textarea id="repos" rows={3} placeholder="acme/api" value={form.repos} onChange={(e) => setForm({ ...form, repos: e.target.value })} />
          </Field>
          {gapLines.length > 0 && (
            <Alert kind="warning">
              {gapLines.join(" ")} A run fails until a code host serves each repository. You can create the factory now and connect one under{" "}
              <Link to="/integrations">Integrations</Link>.
            </Alert>
          )}
        </Card>
        <Card title="Agents">
          <p className="muted small">The foreman is always included. Choose the stage agents it can hand work to.</p>
          <fieldset className="option-list">
            <legend className="sr-only">Agents</legend>
            {AGENT_CATALOG.map((a) => (
              <label key={a.name} className="check">
                <input type="checkbox" checked={agents.includes(a.name)} onChange={() => toggle(agents, a.name, setAgents)} />
                <span>
                  <strong>{a.label}</strong>
                  <span className="muted small">{a.description}</span>
                </span>
              </label>
            ))}
          </fieldset>
        </Card>
        <Card title="Integrations">
          <fieldset className="option-list">
            <legend className="sr-only">Integrations</legend>
            {INTEGRATION_TYPES.map((it) => (
              <label key={it.type} className="check">
                <input type="checkbox" checked={integrations.includes(it.type)} onChange={() => toggle(integrations, it.type, setIntegrations)} />
                <span>
                  <strong>{it.label}</strong>
                  <span className="muted small">{it.hint}</span>
                </span>
              </label>
            ))}
          </fieldset>
        </Card>
        <div className="form-actions">
          <Link to="/factories" className="btn btn-ghost">
            Cancel
          </Link>
          <Button type="submit" variant="primary" busy={create.isPending}>
            Create factory
          </Button>
        </div>
      </form>
    </>
  );
}

/** The page form of the "New work item" dialog (/chat). */
export function ChatPage() {
  const [params] = useSearchParams();
  return (
    <>
      <PageHeader title="New work item" />
      <div className="narrow composer-card">
        <ComposerForm factoryID={params.get("factory") ?? undefined} autoFocus />
      </div>
    </>
  );
}

export { useDispatch };
