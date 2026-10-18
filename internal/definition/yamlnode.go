package definition

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

// diag collects validation errors for a compile.
type diag struct {
	errs []core.ValidationError
}

func (d *diag) add(file string, line int, field, format string, args ...any) {
	d.errs = append(d.errs, core.ValidationError{File: file, Line: line, Field: field, Message: fmt.Sprintf(format, args...)})
}

func (d *diag) sorted() []core.ValidationError {
	out := append([]core.ValidationError(nil), d.errs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// doc is one YAML document being decoded: the file it came from and the line
// offset of its first line (non-zero for Markdown frontmatter).
type doc struct {
	d      *diag
	file   string
	offset int
}

func (c *doc) line(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	return n.Line + c.offset
}

func (c *doc) errf(n *yaml.Node, field, format string, args ...any) {
	c.d.add(c.file, c.line(n), field, format, args...)
}

// parse parses YAML content into its root node. An empty document yields an
// empty mapping node.
func (c *doc) parse(content string) *yaml.Node {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		line := 0
		msg := err.Error()
		// yaml.v3 errors look like "yaml: line 3: ..."
		if rest, ok := strings.CutPrefix(msg, "yaml: line "); ok {
			if num, tail, ok := strings.Cut(rest, ":"); ok {
				if n, err := strconv.Atoi(num); err == nil {
					line = n + c.offset
					msg = "invalid YAML:" + tail
				}
			}
		}
		c.d.add(c.file, line, "", "%s", msg)
		return nil
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Line: 1}
	}
	return root.Content[0]
}

func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func isSeq(n *yaml.Node) bool {
	n = resolve(n)
	return n != nil && n.Kind == yaml.SequenceNode
}

func isNull(n *yaml.Node) bool {
	n = resolve(n)
	return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

func joinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func indexPath(base string, i int) string { return fmt.Sprintf("%s[%d]", base, i) }

// mapping is a YAML mapping being decoded; every key must be consumed or it is
// reported as unknown by finish.
type mapping struct {
	c      *doc
	node   *yaml.Node
	path   string
	order  []string
	vals   map[string]*yaml.Node
	keys   map[string]*yaml.Node
	used   map[string]bool
	strict bool
}

// mapping decodes n as a mapping. It reports an error and returns nil if n is
// not a mapping.
func (c *doc) mapping(n *yaml.Node, path string) *mapping {
	n = resolve(n)
	if n == nil || n.Kind != yaml.MappingNode {
		c.errf(n, path, "%s must be an object", describe(path))
		return nil
	}
	m := &mapping{c: c, node: n, path: path, vals: map[string]*yaml.Node{}, keys: map[string]*yaml.Node{}, used: map[string]bool{}, strict: true}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := resolve(n.Content[i]), n.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Tag == "!!merge" {
			c.errf(k, path, "YAML merge keys (<<) are not supported")
			continue
		}
		if k.Kind != yaml.ScalarNode {
			c.errf(k, path, "%s keys must be strings", describe(path))
			continue
		}
		if _, dup := m.vals[k.Value]; dup {
			c.errf(k, joinPath(path, k.Value), "duplicate key %q", k.Value)
			continue
		}
		m.order = append(m.order, k.Value)
		m.vals[k.Value] = v
		m.keys[k.Value] = k
	}
	return m
}

func describe(path string) string {
	if path == "" {
		return "document"
	}
	return path
}

func (m *mapping) field(k string) string { return joinPath(m.path, k) }

func (m *mapping) has(k string) bool { _, ok := m.vals[k]; return ok }

// get returns the value node for k (nil if absent) and marks it consumed.
func (m *mapping) get(k string) *yaml.Node {
	v, ok := m.vals[k]
	if !ok {
		return nil
	}
	m.used[k] = true
	return v
}

// present reports whether k is present with a non-null value.
func (m *mapping) present(k string) bool {
	v, ok := m.vals[k]
	return ok && !isNull(v)
}

func (m *mapping) keyNode(k string) *yaml.Node {
	if n, ok := m.keys[k]; ok {
		return n
	}
	return m.node
}

func (m *mapping) require(k string) *yaml.Node {
	v := m.get(k)
	if v == nil || isNull(v) {
		m.c.errf(m.node, m.field(k), "%s is required", m.field(k))
		return nil
	}
	return v
}

// finish reports every key that was not consumed.
func (m *mapping) finish() {
	if m == nil {
		return
	}
	for _, k := range m.order {
		if !m.used[k] {
			m.c.errf(m.keys[k], m.field(k), "unknown key %q", m.field(k))
		}
	}
}

// ---- scalar decoders ----

func (c *doc) str(n *yaml.Node, path string) (string, bool) {
	n = resolve(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		c.errf(n, path, "%s must be a string%s", path, quoteHint(n))
		return "", false
	}
	return n.Value, true
}

func quoteHint(n *yaml.Node) string {
	if n != nil && n.Kind == yaml.ScalarNode && (n.Tag == "!!int" || n.Tag == "!!float" || n.Tag == "!!bool") {
		return fmt.Sprintf(" (quote %s to keep it a string)", n.Value)
	}
	return ""
}

// nonEmptyStr decodes a required non-blank string.
func (c *doc) nonEmptyStr(n *yaml.Node, path string) (string, bool) {
	s, ok := c.str(n, path)
	if !ok {
		return "", false
	}
	if strings.TrimSpace(s) == "" {
		c.errf(n, path, "%s must not be empty", path)
		return "", false
	}
	return s, true
}

func (c *doc) boolean(n *yaml.Node, path string) (bool, bool) {
	n = resolve(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
		c.errf(n, path, "%s must be true or false", path)
		return false, false
	}
	var b bool
	if err := n.Decode(&b); err != nil {
		c.errf(n, path, "%s must be true or false", path)
		return false, false
	}
	return b, true
}

func (c *doc) integer(n *yaml.Node, path string) (int, bool) {
	n = resolve(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag != "!!int" {
		c.errf(n, path, "%s must be an integer", path)
		return 0, false
	}
	var i int
	if err := n.Decode(&i); err != nil {
		c.errf(n, path, "%s must be an integer", path)
		return 0, false
	}
	return i, true
}

func (c *doc) number(n *yaml.Node, path string) (float64, bool) {
	n = resolve(n)
	if n == nil || n.Kind != yaml.ScalarNode || (n.Tag != "!!int" && n.Tag != "!!float") {
		c.errf(n, path, "%s must be a number", path)
		return 0, false
	}
	var f float64
	if err := n.Decode(&f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		c.errf(n, path, "%s must be a finite number", path)
		return 0, false
	}
	return f, true
}

func (c *doc) intRange(n *yaml.Node, path string, lo, hi int) (int, bool) {
	i, ok := c.integer(n, path)
	if !ok {
		return 0, false
	}
	if i < lo || i > hi {
		c.errf(n, path, "%s must be between %d and %d", path, lo, hi)
		return 0, false
	}
	return i, true
}

func (c *doc) enum(n *yaml.Node, path string, allowed ...string) (string, bool) {
	s, ok := c.str(n, path)
	if !ok {
		return "", false
	}
	for _, a := range allowed {
		if s == a {
			return s, true
		}
	}
	c.errf(n, path, "%s must be one of %s (got %q)", path, strings.Join(allowed, ", "), s)
	return "", false
}

func (c *doc) seq(n *yaml.Node, path string) []*yaml.Node {
	n = resolve(n)
	if n == nil || n.Kind != yaml.SequenceNode {
		c.errf(n, path, "%s must be a list", path)
		return nil
	}
	return n.Content
}

// strList decodes a list of strings. It returns a non-nil slice for an empty list.
func (c *doc) strList(n *yaml.Node, path string) ([]string, bool) {
	items := c.seq(n, path)
	if items == nil && (resolve(n) == nil || resolve(n).Kind != yaml.SequenceNode) {
		return nil, false
	}
	out := make([]string, 0, len(items))
	ok := true
	for i, it := range items {
		s, good := c.nonEmptyStr(it, indexPath(path, i))
		if !good {
			ok = false
			continue
		}
		out = append(out, s)
	}
	return out, ok
}

// uniqueStrList decodes a list of strings and rejects duplicates.
func (c *doc) uniqueStrList(n *yaml.Node, path string) ([]string, bool) {
	list, ok := c.strList(n, path)
	if !ok {
		return list, false
	}
	seen := map[string]bool{}
	items := resolve(n).Content
	for i, s := range list {
		if seen[s] {
			c.errf(items[i], indexPath(path, i), "%s lists %q more than once", path, s)
			ok = false
		}
		seen[s] = true
	}
	return list, ok
}

// strMap decodes a mapping of string to string.
func (c *doc) strMap(n *yaml.Node, path string) (map[string]string, bool) {
	m := c.mapping(n, path)
	if m == nil {
		return nil, false
	}
	out := map[string]string{}
	ok := true
	for _, k := range m.order {
		v := m.get(k)
		s, good := c.str(v, m.field(k))
		if !good {
			ok = false
			continue
		}
		out[k] = s
	}
	return out, ok
}

// toValue converts a node into a JSON-compatible Go value.
func toValue(n *yaml.Node) any {
	n = resolve(n)
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			m[resolve(n.Content[i]).Value] = toValue(n.Content[i+1])
		}
		return m
	case yaml.SequenceNode:
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			s = append(s, toValue(c))
		}
		return s
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!null":
			return nil
		case "!!bool":
			var b bool
			_ = n.Decode(&b)
			return b
		case "!!int":
			var i int64
			if err := n.Decode(&i); err == nil {
				return i
			}
			var f float64
			_ = n.Decode(&f)
			return f
		case "!!float":
			var f float64
			_ = n.Decode(&f)
			return f
		default:
			return n.Value
		}
	}
	return nil
}

// splitFrontmatter splits a Markdown document into its YAML frontmatter and
// body. hasFM is false when the document does not start with a "---" line.
// fmOffset is the number of lines before the first frontmatter line.
func splitFrontmatter(content string) (fm, body string, hasFM bool, fmOffset int, err error) {
	s := strings.TrimPrefix(content, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if first, _, _ := strings.Cut(s, "\n"); strings.TrimRight(first, " \t") != "---" {
		return "", s, false, 0, nil
	}
	lines := strings.Split(s, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			fm = strings.Join(lines[1:i], "\n")
			body = strings.Join(lines[i+1:], "\n")
			return fm, body, true, 1, nil
		}
	}
	return "", "", true, 1, fmt.Errorf("frontmatter starting on line 1 is not closed by a \"---\" line")
}
