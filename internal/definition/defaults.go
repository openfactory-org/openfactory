package definition

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

//go:embed defaults/agents
var defaultAgentsFS embed.FS

// DefaultAgentNames lists the default agents in pipeline order.
var DefaultAgentNames = []string{"foreman", "triage", "spec", "implement", "review"}

// DefaultModel is the factory-wide default model of a new factory.
const DefaultModel = "claude-opus-5-5"

// DefaultAgents returns the default agent files of a new factory, keyed by
// definition path (agents/<name>/agent.md).
func DefaultAgents() map[string]string {
	out := map[string]string{}
	err := fs.WalkDir(defaultAgentsFS, "defaults/agents", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := defaultAgentsFS.ReadFile(p)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(p, "defaults/")] = string(b)
		return nil
	})
	if err != nil {
		panic("definition: reading embedded default agents: " + err.Error())
	}
	return out
}

// DefaultDefinitionOptions configures DefaultDefinition.
type DefaultDefinitionOptions struct {
	Name         string
	Description  string
	Alias        string
	Repositories []core.RepoRef
	Model        string   // default DefaultModel
	Agents       []string // subset of DefaultAgentNames; foreman is always included. Empty = all.
}

// DefaultDefinition returns a complete, valid definition tree for a new
// factory: factory.yaml plus the selected default agents.
func DefaultDefinition(opts DefaultDefinitionOptions) (map[string]string, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, fmt.Errorf("factory name is required")
	}
	model := opts.Model
	if model == "" {
		model = DefaultModel
	}
	type repo struct {
		CodeForge string `yaml:"codeForge,omitempty"`
		Owner     string `yaml:"owner"`
		Name      string `yaml:"name"`
	}
	type factoryYAML struct {
		SchemaVersion string            `yaml:"schemaVersion"`
		Name          string            `yaml:"name"`
		Description   string            `yaml:"description,omitempty"`
		Alias         string            `yaml:"alias,omitempty"`
		Repositories  []repo            `yaml:"repositories"`
		AgentDefaults map[string]string `yaml:"agentDefaults"`
	}
	fy := factoryYAML{SchemaVersion: SchemaVersion, Name: opts.Name, Description: opts.Description, Alias: opts.Alias,
		Repositories: []repo{}, AgentDefaults: map[string]string{"model": model}}
	forges := map[string]bool{}
	for _, r := range opts.Repositories {
		forges[r.CodeForge] = true
	}
	for _, r := range opts.Repositories {
		rr := repo{Owner: r.Owner, Name: r.Name}
		if len(forges) > 1 || (r.CodeForge != "" && r.CodeForge != "GITHUB") {
			rr.CodeForge = r.CodeForge
		}
		fy.Repositories = append(fy.Repositories, rr)
	}
	b, err := yaml.Marshal(fy)
	if err != nil {
		return nil, err
	}
	files := map[string]string{factoryFile: string(b)}
	if len(forges) > 1 || (len(forges) == 1 && !forges["GITHUB"] && !forges[""]) {
		var list []string
		for _, f := range codeForgeValues {
			if forges[f] {
				list = append(list, f)
			}
		}
		if len(list) > 0 {
			files[factoryFile] += "codeForges:\n"
			for _, f := range list {
				files[factoryFile] += "  - " + f + "\n"
			}
		}
	}
	want := map[string]bool{"foreman": true}
	if len(opts.Agents) == 0 {
		for _, n := range DefaultAgentNames {
			want[n] = true
		}
	}
	for _, n := range opts.Agents {
		if !contains(DefaultAgentNames, n) {
			return nil, fmt.Errorf("unknown default agent %q", n)
		}
		want[n] = true
	}
	for p, content := range DefaultAgents() {
		name := strings.Split(p, "/")[1]
		if want[name] {
			files[p] = content
		}
	}
	return files, nil
}

// LoadDir reads a definition tree from disk, returning slash-separated paths
// relative to root. VCS metadata and hidden directories are skipped.
func LoadDir(root string) (map[string]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	const maxFile = 4 << 20
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	files := map[string]string{}
	err = fs.WalkDir(dir.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != "." && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.Size() > maxFile {
			return fmt.Errorf("%s is larger than %d bytes", filepath.Join(root, p), maxFile)
		}
		b, err := dir.ReadFile(p)
		if err != nil {
			return err
		}
		files[p] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
