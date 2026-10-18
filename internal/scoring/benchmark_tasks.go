package scoring

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TaskFromRunRequest makes a benchmark task from a finished run.
type TaskFromRunRequest struct {
	FactoryID string
	Suite     string
	RunID     string
	// Slug is the task slug. Default: a slug from the run title or ID.
	Slug string
	// Title is the task title. Default: the run name and ID.
	Title string
	// SuccessCriteria says what a successful trial does. Required.
	SuccessCriteria string
	Tags            []string
}

// TaskFromRun is the result of MakeTaskFromRun.
type TaskFromRun struct {
	Task core.BenchmarkTask `json:"task"`
	// Path is the path of the new task file.
	Path string `json:"path"`
	// Files are the changed files of the definition: the task file and the
	// suite file with the task in its list.
	Files map[string]string `json:"files"`
	// Definition is the full definition tree with the change.
	Definition map[string]string `json:"-"`
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// MakeTaskFromRun builds a benchmark task from a finished run: the prompt of
// the run, and the commits that the run checked out as the start state. It
// does not apply the change.
func (s *Service) MakeTaskFromRun(ctx context.Context, req TaskFromRunRequest) (*TaskFromRun, error) {
	if strings.TrimSpace(req.SuccessCriteria) == "" {
		return nil, fmt.Errorf("%w: success_criteria is required", core.ErrInvalid)
	}
	run, err := s.opts.Store.GetRun(ctx, req.RunID)
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", req.RunID, err)
	}
	if run.FactoryID != req.FactoryID {
		return nil, fmt.Errorf("run %s: %w", req.RunID, core.ErrNotFound)
	}
	if !run.State.Terminal() && !run.State.WaitsForInput() {
		return nil, fmt.Errorf("%w: run %s is not finished", core.ErrConflict, run.ID)
	}
	if strings.TrimSpace(run.Prompt) == "" {
		return nil, fmt.Errorf("%w: run %s has no prompt", core.ErrInvalid, run.ID)
	}
	_, v, def, err := s.activeDefinition(ctx, req.FactoryID)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("%w: the factory has no active definition", core.ErrInvalid)
	}
	suiteSlug := req.Suite
	if def != nil {
		if suite, ok := findSuite(def.Benchmarks, req.Suite); ok {
			suiteSlug = suite.Slug
		}
	}
	root := definitionRoot(v.Files)
	suitePath := root + "benchmarks/" + suiteSlug + "/suite.yaml"
	suiteYAML, ok := v.Files[suitePath]
	if !ok {
		return nil, fmt.Errorf("benchmark suite %q: %w", req.Suite, core.ErrNotFound)
	}
	starts, err := core.LoadStartCommits(ctx, s.opts.Store, run.ID)
	if err != nil {
		return nil, err
	}
	task := core.BenchmarkTask{
		Title:           strings.TrimSpace(req.Title),
		Prompt:          strings.TrimSpace(run.Prompt),
		SuccessCriteria: strings.TrimSpace(req.SuccessCriteria),
		SourceRunID:     run.ID,
		Tags:            req.Tags,
	}
	var refs []string
	for _, st := range starts {
		if !st.Resolved || !shaRe.MatchString(st.Ref) {
			return nil, fmt.Errorf("%w: run %s has no recorded start commit for %s/%s (only runs on the runner path record the commits they check out)", core.ErrInvalid, run.ID, st.Owner, st.Repo)
		}
		ref := st.RepoAtCommit()
		forge := strings.ToUpper(ref.CodeForge)
		if forge != "GITHUB" && forge != "GITLAB" {
			return nil, fmt.Errorf("%w: benchmark tasks support GitHub and GitLab repositories only (%s/%s is on %s)", core.ErrInvalid, ref.Owner, ref.Repo, ref.CodeForge)
		}
		ref.CodeForge = forge
		task.StartingRepoRefs = append(task.StartingRepoRefs, ref)
		refs = append(refs, FormatRepoAtCommit(ref))
	}
	if task.Title == "" {
		name := run.Name
		if name == "" {
			name = run.AgentName
		}
		task.Title = strings.TrimSpace(fmt.Sprintf("%s %s", name, run.ID))
	}
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(task.Title), "-"), "-")
		if len(slug) > 50 {
			slug = strings.Trim(slug[:50], "-")
		}
		if slug == "" {
			slug = "task-" + strings.ToLower(run.ID)
		}
	}
	task.Slug = slug
	taskPath := root + "benchmarks/" + suiteSlug + "/tasks/" + slug + ".yaml"
	if _, exists := v.Files[taskPath]; exists {
		return nil, fmt.Errorf("%w: task %q already exists in suite %q", core.ErrConflict, slug, suiteSlug)
	}
	doc := map[string]any{
		"title":           task.Title,
		"prompt":          task.Prompt,
		"successCriteria": task.SuccessCriteria,
		"sourceRunId":     task.SourceRunID,
	}
	if len(task.Tags) > 0 {
		doc["tags"] = task.Tags
	}
	if len(refs) > 0 {
		doc["startingRepoRefs"] = refs
	}
	taskYAML, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	newSuite, err := addSuiteTask(suiteYAML, slug)
	if err != nil {
		return nil, err
	}
	files := map[string]string{taskPath: string(taskYAML), suitePath: newSuite}
	full := make(map[string]string, len(v.Files)+1)
	for p, c := range v.Files {
		full[p] = c
	}
	for p, c := range files {
		full[p] = c
	}
	return &TaskFromRun{Task: task, Path: taskPath, Files: files, Definition: full}, nil
}

// addSuiteTask appends slug to the tasks list of a suite.yaml document.
func addSuiteTask(content, slug string) (string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		return "", fmt.Errorf("%w: suite.yaml: %w", core.ErrInvalid, err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("%w: suite.yaml is not a mapping", core.ErrInvalid)
	}
	m := root.Content[0]
	item := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: slug}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "tasks" {
			continue
		}
		list := m.Content[i+1]
		if list.Kind != yaml.SequenceNode {
			list.Kind, list.Tag, list.Value, list.Content = yaml.SequenceNode, "!!seq", "", nil
		}
		list.Content = append(list.Content, item)
		return encodeYAML(&root)
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "tasks"},
		&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}})
	return encodeYAML(&root)
}

func encodeYAML(n *yaml.Node) (string, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
