package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/api"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + ":" + id))
}

var (
	issueKeyRe  = regexp.MustCompile(`^[A-Z][A-Z0-9]+-\d+$`)
	shortRefRe  = regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)$`)
	slackPathRe = regexp.MustCompile(`^/archives/([A-Z0-9]+)/p(\d{10})(\d{6})$`)
)

// resolveWorkItem finds the work item a reference points at. References may be a
// work item ID, a run ID, a dashboard URL, a pull request / issue URL, an
// owner/repo#N short reference, a Slack permalink, a Linear or Jira issue key
// or URL, or a branch name.
func (s *Server) resolveWorkItem(ctx context.Context, p *api.Principal, ref string) (*core.WorkItem, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, invalid("work item is required")
	}
	notFound := api.Errf(api.CodeResourceNotFound, "no work item matches %q", ref)

	if wi, err := s.ownedWorkItem(ctx, p, ref); err == nil {
		return wi, nil
	}
	if wi := s.workItemOfRun(ctx, p, ref); wi != nil {
		return wi, nil
	}

	var keys []string
	var prURL string
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
		path := strings.TrimRight(u.Path, "/")
		host := strings.ToLower(u.Host)
		segs := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case s.opts.PublicURL != "" && strings.HasPrefix(ref, s.opts.PublicURL):
			for i := 0; i+1 < len(segs); i++ {
				switch segs[i] {
				case "work", "work-items":
					if wi, err := s.ownedWorkItem(ctx, p, segs[i+1]); err == nil {
						return wi, nil
					}
				case "runs":
					if wi := s.workItemOfRun(ctx, p, segs[i+1]); wi != nil {
						return wi, nil
					}
				}
			}
			return nil, notFound
		case strings.HasSuffix(host, "slack.com"):
			if m := slackPathRe.FindStringSubmatch(path); m != nil {
				ts := m[2] + "." + m[3]
				if tts := u.Query().Get("thread_ts"); tts != "" {
					ts = tts
				}
				keys = append(keys, "slack:"+m[1]+":"+ts)
			}
		case host == "linear.app":
			for i := 0; i+1 < len(segs); i++ {
				if segs[i] == "issue" {
					keys = append(keys, "linear:"+segs[i+1])
				}
			}
		case strings.Contains(path, "/browse/"):
			keys = append(keys, "jira:"+segs[len(segs)-1])
		default:
			if pr, ok := parsePullRequestURL(ref); ok {
				prURL = pr.URL
				keys = append(keys, strings.ToLower(pr.Forge)+":"+pr.Repo+"#"+strconv.Itoa(pr.Number))
			} else if forge, repo, n, ok := parseIssueURL(u); ok {
				keys = append(keys, forge+":"+repo+"#"+strconv.Itoa(n))
			}
		}
		if len(keys) == 0 && prURL == "" {
			return nil, notFound
		}
	} else if m := shortRefRe.FindStringSubmatch(ref); m != nil {
		keys = append(keys, "github:"+m[1]+"#"+m[2], "gitlab:"+m[1]+"#"+m[2])
	} else if issueKeyRe.MatchString(ref) {
		keys = append(keys, "linear:"+ref, "jira:"+ref)
	}

	fs, err := s.opts.Store.ListFactories(ctx, p.TeamID, "")
	if err != nil {
		return nil, err
	}
	// Fast path: most recent non-terminal work item by source key.
	for _, key := range keys {
		for _, f := range fs {
			if wi, err := s.opts.Store.FindWorkItemBySource(ctx, f.ID, key); err == nil {
				return wi, nil
			}
		}
	}
	// Slow path: scan recent work items (including terminal ones), matching
	// source keys, linked pull requests and branches.
	branch := ""
	if len(keys) == 0 && prURL == "" {
		branch = ref
	}
	var best *core.WorkItem
	for _, f := range fs {
		items, err := s.opts.Store.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, ListOpts: core.ListOpts{Limit: 500}})
		if err != nil {
			return nil, err
		}
		for i := range items {
			wi := &items[i]
			if matchesWorkItem(wi, keys, prURL, branch) && (best == nil || wi.UpdatedAt.After(best.UpdatedAt)) {
				best = wi
			}
		}
	}
	if best == nil {
		return nil, notFound
	}
	return best, nil
}

func matchesWorkItem(wi *core.WorkItem, keys []string, prURL, branch string) bool {
	sk := wi.SourceKey
	if sk == "" {
		sk = wi.Source.Key()
	}
	for _, k := range keys {
		if strings.EqualFold(sk, k) {
			return true
		}
	}
	for _, pr := range wi.PullRequests {
		if prURL != "" && strings.EqualFold(strings.TrimRight(pr.URL, "/"), strings.TrimRight(prURL, "/")) {
			return true
		}
		for _, k := range keys {
			if strings.EqualFold(strings.ToLower(pr.Forge)+":"+pr.Repo+"#"+strconv.Itoa(pr.Number), k) {
				return true
			}
		}
		if branch != "" && pr.Branch == branch {
			return true
		}
	}
	return false
}

func (s *Server) ownedWorkItem(ctx context.Context, p *api.Principal, id string) (*core.WorkItem, error) {
	wi, err := s.opts.Store.GetWorkItem(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.factoryOf(ctx, p, wi.FactoryID); err != nil {
		return nil, err
	}
	return wi, nil
}

func (s *Server) workItemOfRun(ctx context.Context, p *api.Principal, runID string) *core.WorkItem {
	run, err := s.opts.Store.GetRun(ctx, runID)
	if err != nil || run.TeamID != p.TeamID || run.WorkItemID == "" {
		return nil
	}
	wi, err := s.ownedWorkItem(ctx, p, run.WorkItemID)
	if err != nil {
		return nil
	}
	return wi
}

// parsePullRequestURL recognizes GitHub pull requests, GitLab merge requests
// and Azure DevOps pull requests.
func parsePullRequestURL(raw string) (core.PullRequestRef, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return core.PullRequestRef{}, false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	host := strings.ToLower(u.Host)
	ref := core.PullRequestRef{URL: strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), State: "open"}
	switch {
	case strings.Contains(host, "dev.azure.com") || strings.Contains(host, "visualstudio.com"):
		// /{org}/{project}/_git/{repo}/pullrequest/{n}
		for i := 0; i+1 < len(segs); i++ {
			if segs[i] == "pullrequest" && i >= 2 && segs[i-2] == "_git" {
				n, err := strconv.Atoi(segs[i+1])
				if err != nil {
					return ref, false
				}
				ref.Forge, ref.Repo, ref.Number = "AZURE_DEVOPS", strings.Join(segs[:i-2], "/")+"/"+segs[i-1], n
				return ref, true
			}
		}
	case len(segs) >= 4 && segs[len(segs)-2] == "pull":
		n, err := strconv.Atoi(segs[len(segs)-1])
		if err != nil {
			return ref, false
		}
		ref.Forge, ref.Repo, ref.Number = "GITHUB", strings.Join(segs[:len(segs)-2], "/"), n
		return ref, true
	case len(segs) >= 5 && segs[len(segs)-3] == "-" && segs[len(segs)-2] == "merge_requests":
		n, err := strconv.Atoi(segs[len(segs)-1])
		if err != nil {
			return ref, false
		}
		ref.Forge, ref.Repo, ref.Number = "GITLAB", strings.Join(segs[:len(segs)-3], "/"), n
		return ref, true
	}
	return ref, false
}

func parseIssueURL(u *url.URL) (forge, repo string, n int, ok bool) {
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) >= 4 && segs[len(segs)-2] == "issues" {
		num, err := strconv.Atoi(segs[len(segs)-1])
		if err != nil {
			return "", "", 0, false
		}
		if len(segs) >= 5 && segs[len(segs)-3] == "-" {
			return "gitlab", strings.Join(segs[:len(segs)-3], "/"), num, true
		}
		return "github", strings.Join(segs[:len(segs)-2], "/"), num, true
	}
	return "", "", 0, false
}

// ---------- start working ----------

type repoSetup struct {
	Repository string   `json:"repository"`
	CloneURL   string   `json:"clone_url"`
	Branch     string   `json:"branch"`
	BaseBranch string   `json:"base_branch,omitempty"`
	Worktree   string   `json:"worktree"`
	Commands   []string `json:"commands"`
	Existing   bool     `json:"existing_branch"`
}

func cloneURL(forge, owner, name string) string {
	switch strings.ToUpper(forge) {
	case "GITLAB":
		return "https://gitlab.com/" + owner + "/" + name + ".git"
	case "AZURE_DEVOPS":
		org, project, _ := strings.Cut(owner, "/")
		if project == "" {
			project = org
		}
		return "https://dev.azure.com/" + org + "/" + project + "/_git/" + name
	default:
		return "https://github.com/" + owner + "/" + name + ".git"
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func shortID(id string) string {
	if i := strings.LastIndex(id, "_"); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// startWorking marks the work item as taken over locally and returns Git setup guidance.
func (s *Server) startWorking(ctx context.Context, p *api.Principal, wi *core.WorkItem, activeRuns int) (map[string]any, error) {
	cur, err := storeutil.UpdateWorkItem(ctx, s.opts.Store, wi.ID, func(cur *core.WorkItem) error {
		cur.LocalOwner = p.UserID()
		cur.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return nil, err
	}
	*wi = *cur
	_ = s.opts.Store.AppendWorkItemEvent(ctx, &core.WorkItemEvent{ID: core.NewID("wie"), WorkItemID: wi.ID, Kind: "note",
		Actor: p.ActorID(), Data: mustJSON(map[string]any{"local_owner": p.UserID(), "picked_up_locally": true}), CreatedAt: time.Now().UTC()})

	sid := shortID(wi.ID)
	newBranch := "factory/" + sid
	if sl := slug(wi.Title); sl != "" {
		newBranch += "-" + sl
	}
	var setups []repoSetup
	seen := map[string]bool{}
	for _, pr := range wi.PullRequests {
		if pr.Branch == "" || seen[pr.Repo] || pr.State == "merged" || pr.State == "closed" {
			continue
		}
		seen[pr.Repo] = true
		owner, name := splitRepo(pr.Repo)
		setups = append(setups, repoSetupFor(pr.Forge, owner, name, pr.Branch, "", sid, true))
	}
	if len(setups) == 0 {
		repos := s.factoryRepos(ctx, wi.FactoryID)
		if wi.Source.Repo != "" {
			owner, name := splitRepo(wi.Source.Repo)
			forge := "GITHUB"
			if wi.Source.Provider == "gitlab" {
				forge = "GITLAB"
			}
			repos = append([]core.RepoRef{{CodeForge: forge, Owner: owner, Name: name}}, repos...)
		}
		for _, r := range repos {
			if seen[r.FullName()] {
				continue
			}
			seen[r.FullName()] = true
			setups = append(setups, repoSetupFor(r.CodeForge, r.Owner, r.Name, newBranch, "main", sid, false))
		}
	}
	guidance := map[string]any{
		"local_owner":  p.UserID(),
		"repositories": setups,
		"warnings": []string{
			"Picking up a work item does not claim, lock, or pause it: the factory may keep running its own work on it.",
		},
		"next_steps": []string{
			"Run the setup commands yourself; Factory MCP never modifies your files.",
			"Tell the foreman you are picking the work item up with message_foreman, and read replies with get_conversation.",
			"Validate your change, commit and push the branch: the factory cannot see uncommitted or unpushed work.",
			"Hand back with send_work_item using work_item_id, the pushed branch or pull_request_url, and a note covering what changed, what was validated, and what remains.",
			"When nothing remains for the factory to do, call complete_work_item.",
		},
	}
	if activeRuns > 0 {
		guidance["warnings"] = append(guidance["warnings"].([]string),
			strconv.Itoa(activeRuns)+" run(s) are still active on this work item; coordinate with the foreman to avoid duplicate changes.")
	}
	if len(setups) == 0 {
		guidance["warnings"] = append(guidance["warnings"].([]string),
			"No repository is linked to this work item or its factory; ask the foreman which repository to use.")
	}
	return guidance, nil
}

func repoSetupFor(forge, owner, name, branch, base, sid string, existing bool) repoSetup {
	u := cloneURL(forge, owner, name)
	wt := "../" + name + "-" + sid
	cmds := []string{
		"git clone " + u + " " + name + "  # skip if you already have a clone",
		"cd " + name,
		"git fetch origin",
	}
	if existing {
		cmds = append(cmds, "git worktree add -B "+branch+" "+wt+" origin/"+branch)
	} else {
		cmds = append(cmds, "git worktree add -b "+branch+" "+wt+" origin/HEAD  # or origin/"+base)
	}
	cmds = append(cmds, "cd "+wt)
	return repoSetup{Repository: owner + "/" + name, CloneURL: u, Branch: branch, BaseBranch: map[bool]string{true: "", false: base}[existing],
		Worktree: wt, Commands: cmds, Existing: existing}
}

func splitRepo(full string) (string, string) {
	i := strings.LastIndex(full, "/")
	if i < 0 {
		return "", full
	}
	return full[:i], full[i+1:]
}

func (s *Server) factoryRepos(ctx context.Context, factoryID string) []core.RepoRef {
	f, err := s.opts.Store.GetFactory(ctx, factoryID)
	if err != nil || f.ActiveVersionID == "" || s.opts.Compile == nil {
		return nil
	}
	v, err := s.opts.Store.GetDefinitionVersion(ctx, f.ActiveVersionID)
	if err != nil {
		return nil
	}
	def, _ := s.opts.Compile(v.Files)
	if def == nil {
		return nil
	}
	return def.Repositories
}
