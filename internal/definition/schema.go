package definition

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// SchemaBaseURL is the base of every schema document's $id.
const SchemaBaseURL = "https://openfactory-org.github.io/schemas/" + SchemaVersion + "/"

//go:embed schemas/v1/*.schema.json
var schemaFS embed.FS

// SchemaNames lists the v1 schema documents (without the ".schema.json" suffix).
var SchemaNames = []string{"factory", "agent", "automation", "scorer", "machine", "webhook", "benchmark_suite", "benchmark_suite_task", "common"}

// SchemaCatalog describes the supported schema versions.
type SchemaCatalog struct {
	Current  string   `json:"current"`
	Versions []string `json:"versions"`
}

// SchemaVersions returns the supported schema versions and the current one.
func SchemaVersions() SchemaCatalog {
	return SchemaCatalog{Current: SchemaVersion, Versions: []string{SchemaVersion}}
}

// Schemas returns every v1 JSON Schema document keyed by document name
// (for example "factory.schema.json").
func Schemas() map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, n := range SchemaNames {
		b, _ := Schema(n)
		out[n+".schema.json"] = b
	}
	return out
}

// Schema returns one schema document. name may be "factory",
// "factory.schema.json" or "v1/factory.schema.json".
func Schema(name string) (json.RawMessage, bool) {
	name = strings.TrimPrefix(name, SchemaVersion+"/")
	name = strings.TrimSuffix(name, ".json")
	name = strings.TrimSuffix(name, ".schema")
	b, err := schemaFS.ReadFile("schemas/" + SchemaVersion + "/" + name + ".schema.json")
	if err != nil {
		return nil, false
	}
	return json.RawMessage(b), true
}

// SchemaForPath returns the schema document name that applies to a definition
// file path, or "" if no schema applies (skills, READMEs, other files).
func SchemaForPath(p string) string {
	p = NormalizePath(p)
	seg := strings.Split(p, "/")
	switch {
	case p == factoryFile:
		return "factory"
	case len(seg) == 3 && seg[0] == "agents" && seg[2] == "agent.md":
		return "agent"
	case len(seg) == 3 && seg[0] == "automations" && seg[2] == "automation.md":
		return "automation"
	case len(seg) == 3 && seg[0] == "scorers" && seg[2] == "scorer.md":
		return "scorer"
	case len(seg) == 2 && seg[0] == "machines" && strings.HasSuffix(p, ".yaml"):
		return "machine"
	case len(seg) == 2 && seg[0] == "webhooks" && strings.HasSuffix(p, ".yaml"):
		return "webhook"
	case len(seg) == 3 && seg[0] == "benchmarks" && seg[2] == "suite.yaml":
		return "benchmark_suite"
	case len(seg) == 4 && seg[0] == "benchmarks" && seg[2] == "tasks" && strings.HasSuffix(p, ".yaml"):
		return "benchmark_suite_task"
	}
	return ""
}

var (
	schemaOnce     sync.Once
	compiledSchema map[string]*jsonschema.Schema
	schemaErr      error
)

func compiledSchemas() (map[string]*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		for _, n := range SchemaNames {
			b, _ := Schema(n)
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
			if err != nil {
				schemaErr = fmt.Errorf("schema %s: %w", n, err)
				return
			}
			if err := c.AddResource(SchemaBaseURL+n+".schema.json", doc); err != nil {
				schemaErr = fmt.Errorf("schema %s: %w", n, err)
				return
			}
		}
		compiledSchema = map[string]*jsonschema.Schema{}
		for _, n := range SchemaNames {
			s, err := c.Compile(SchemaBaseURL + n + ".schema.json")
			if err != nil {
				schemaErr = fmt.Errorf("schema %s: %w", n, err)
				return
			}
			compiledSchema[n] = s
		}
	})
	return compiledSchema, schemaErr
}

// ValidateSchema validates one definition file against its JSON Schema
// (Markdown files are validated through their frontmatter). Files without a
// schema validate trivially. Schema validation is structural; Compile performs
// the complete semantic validation.
func ValidateSchema(p, content string) error {
	name := SchemaForPath(p)
	if name == "" {
		return nil
	}
	schemas, err := compiledSchemas()
	if err != nil {
		return err
	}
	src := content
	if strings.HasSuffix(p, ".md") {
		fm, _, has, _, err := splitFrontmatter(content)
		if err != nil {
			return err
		}
		if !has {
			fm = ""
		}
		src = fm
	}
	var v any
	if err := yaml.Unmarshal([]byte(src), &v); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}
	if v == nil {
		v = map[string]any{}
	}
	raw, err := json.Marshal(jsonCompatible(v))
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return schemas[name].Validate(inst)
}

// jsonCompatible converts YAML-decoded values (which may contain
// map[string]any with non-string keys) into JSON-encodable values.
func jsonCompatible(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = jsonCompatible(x)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[fmt.Sprint(k)] = jsonCompatible(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = jsonCompatible(x)
		}
		return out
	}
	return v
}
