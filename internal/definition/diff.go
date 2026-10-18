package definition

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Change actions.
const (
	ChangeAdded   = "added"
	ChangeRemoved = "removed"
	ChangeChanged = "changed"
)

// Resource kinds reported by Diff.
const (
	KindFactory    = "factory"
	KindAgent      = "agent"
	KindAutomation = "automation"
	KindMachine    = "machine"
	KindScorer     = "scorer"
	KindWebhook    = "webhook"
	KindBenchmark  = "benchmark"
	KindSkill      = "skill"
)

// Change is one added, removed or changed resource.
type Change struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Action string   `json:"action"`
	Fields []string `json:"fields,omitempty"` // changed top-level fields (Action == changed)
}

// DiffResult summarizes the differences between two compiled definitions.
type DiffResult struct {
	Changes []Change `json:"changes"`
}

// Empty reports whether nothing changed.
func (r DiffResult) Empty() bool { return len(r.Changes) == 0 }

// Counts returns the number of added, removed and changed resources.
func (r DiffResult) Counts() (added, removed, changed int) {
	for _, c := range r.Changes {
		switch c.Action {
		case ChangeAdded:
			added++
		case ChangeRemoved:
			removed++
		case ChangeChanged:
			changed++
		}
	}
	return
}

// Summary renders a short human-readable summary, one change per line, for
// pull request checks and the dashboard.
func (r DiffResult) Summary() string {
	if r.Empty() {
		return "No changes to the factory definition."
	}
	a, rm, ch := r.Counts()
	var b strings.Builder
	fmt.Fprintf(&b, "%d added, %d removed, %d changed\n", a, rm, ch)
	sym := map[string]string{ChangeAdded: "+", ChangeRemoved: "-", ChangeChanged: "~"}
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "%s %s %s", sym[c.Action], c.Kind, c.Name)
		if len(c.Fields) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(c.Fields, ", "))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Diff compares two compiled definitions. Either may be nil (treated as an
// empty definition). Changes are ordered by kind, then name.
func Diff(old, new *core.CompiledDefinition) DiffResult {
	if old == nil {
		old = &core.CompiledDefinition{}
	}
	if new == nil {
		new = &core.CompiledDefinition{}
	}
	var changes []Change

	// Factory-level settings (everything except the resource collections).
	if fields := fieldDiff(factoryOnly(old), factoryOnly(new)); len(fields) > 0 {
		name := new.Name
		if name == "" {
			name = old.Name
		}
		changes = append(changes, Change{Kind: KindFactory, Name: name, Action: ChangeChanged, Fields: fields})
	}

	changes = append(changes, diffSet(KindAgent, indexBy(old.Agents, func(a core.AgentConfig) string { return a.Name }), indexBy(new.Agents, func(a core.AgentConfig) string { return a.Name }))...)
	changes = append(changes, diffSet(KindAutomation, indexBy(old.Automations, func(a core.Automation) string { return a.Name }), indexBy(new.Automations, func(a core.Automation) string { return a.Name }))...)
	changes = append(changes, diffSet(KindMachine, old.Machines, new.Machines)...)
	changes = append(changes, diffSet(KindScorer, indexBy(old.Scorers, func(s core.Scorer) string { return s.Slug }), indexBy(new.Scorers, func(s core.Scorer) string { return s.Slug }))...)
	changes = append(changes, diffSet(KindWebhook, indexBy(old.Webhooks, func(w core.Webhook) string { return w.Name }), indexBy(new.Webhooks, func(w core.Webhook) string { return w.Name }))...)
	changes = append(changes, diffSet(KindBenchmark, indexBy(old.Benchmarks, func(b core.BenchmarkSuite) string { return b.Slug }), indexBy(new.Benchmarks, func(b core.BenchmarkSuite) string { return b.Slug }))...)
	changes = append(changes, diffSet(KindSkill, skillIndex(old), skillIndex(new))...)
	return DiffResult{Changes: changes}
}

func factoryOnly(d *core.CompiledDefinition) core.CompiledDefinition {
	c := *d
	c.Agents, c.Automations, c.Machines, c.Scorers, c.Webhooks, c.Benchmarks, c.Skills = nil, nil, nil, nil, nil, nil, nil
	return c
}

// skillIndex indexes factory skills by name and agent skills by "agent/name".
// Agent skills are also reported on their agent's "skills" field.
func skillIndex(d *core.CompiledDefinition) map[string]core.Skill {
	out := map[string]core.Skill{}
	for _, s := range d.Skills {
		out[s.Name] = s
	}
	for _, a := range d.Agents {
		for _, s := range a.Skills {
			out[a.Name+"/"+s.Name] = s
		}
	}
	return out
}

func indexBy[T any](items []T, key func(T) string) map[string]T {
	out := make(map[string]T, len(items))
	for _, it := range items {
		out[key(it)] = it
	}
	return out
}

func diffSet[T any](kind string, old, new map[string]T) []Change {
	names := map[string]bool{}
	for k := range old {
		names[k] = true
	}
	for k := range new {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []Change
	for _, n := range sorted {
		o, inOld := old[n]
		nv, inNew := new[n]
		switch {
		case !inOld:
			out = append(out, Change{Kind: kind, Name: n, Action: ChangeAdded})
		case !inNew:
			out = append(out, Change{Kind: kind, Name: n, Action: ChangeRemoved})
		default:
			if fields := fieldDiff(o, nv); len(fields) > 0 {
				out = append(out, Change{Kind: kind, Name: n, Action: ChangeChanged, Fields: fields})
			}
		}
	}
	return out
}

// fieldDiff returns the JSON field names whose values differ between a and b.
func fieldDiff(a, b any) []string {
	am, bm := toFieldMap(a), toFieldMap(b)
	keys := map[string]bool{}
	for k := range am {
		keys[k] = true
	}
	for k := range bm {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		if !bytes.Equal(am[k], bm[k]) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func toFieldMap(v any) map[string]json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	// Normalize empty collections so nil vs empty is not reported as a change.
	for k, raw := range m {
		switch string(raw) {
		case "null", "[]", "{}", `""`, "false", "0":
			delete(m, k)
		}
	}
	return m
}
