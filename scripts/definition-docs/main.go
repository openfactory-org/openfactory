// Command definition-docs writes the field tables of docs/definition-reference.md
// from the v1 JSON Schemas (internal/definition/schemas/v1), so that the page
// and the schemas cannot drift.
//
//	go run ./scripts/definition-docs           # rewrite the generated sections
//	go run ./scripts/definition-docs -check    # fail when a generated section is stale
//
// Each table sits between the markers
//
//	<!-- BEGIN GENERATED: <id> (make docs-gen) -->
//	<!-- END GENERATED: <id> -->
//
// and <id> names an entry of sections. Text outside the markers is written
// by hand and stays as it is. The schemas are the single source of the field
// names, types, required keys, defaults, allowed values and descriptions.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/definition"
)

// section describes one generated table.
type section struct {
	// doc and pointer select the object schema: a schema document name and a
	// JSON pointer in it ("" is the root).
	doc, pointer string
	// prefix is put in front of each field name (for example "github.").
	prefix string
	// expand lists the object (or list of objects) fields whose fields get
	// their own rows, as dotted paths ("platform.linux", "labels[]" is
	// written "labels").
	expand []string
	// order sorts the rows. A field that is not listed comes after the listed
	// fields of its object, in alphabetical order.
	order []string
}

var sections = map[string]section{
	"factory": {doc: "factory", expand: []string{"spendLimit"}, order: []string{
		"schemaVersion", "name", "description", "alias", "credentialStrategy", "maxConcurrentRuns", "approvers",
		"restrictFollowupsToStarter", "requireLinkedActor", "allowAgentMerge", "spendLimit", "spendLimit.daily", "spendLimit.monthly",
		"codeForges", "repositories", "benchmarkRepoSubstitutions", "secrets", "networkPolicy", "mcpServers",
		"cloudProviders", "integrations", "automations", "github", "scorerDefaults", "selfImprovement", "agentDefaults",
	}},
	"factory.repositories": {doc: "factory", pointer: "/properties/repositories/items", order: []string{"codeForge", "owner", "name"}},
	"factory.benchmarkRepoSubstitutions": {doc: "factory", pointer: "/properties/benchmarkRepoSubstitutions/items",
		expand: []string{"source", "target"}, order: []string{
			"source", "source.codeForge", "source.owner", "source.name", "target", "target.codeForge", "target.owner", "target.name",
		}},
	"factory.cloudProviders": {doc: "factory", pointer: "/properties/cloudProviders", expand: []string{"gcp", "aws"}, order: []string{
		"gcp", "gcp.projectNumber", "gcp.workloadIdentityFederationPoolId", "gcp.workloadIdentityFederationProviderId",
		"gcp.serviceAccountEmail", "aws", "aws.roleArn", "audiences",
	}},
	"factory.integrations": {doc: "factory", pointer: "/properties/integrations/anyOf/0/items",
		expand: []string{"slack", "microsoft-teams", "linear", "jira"},
		order:  []string{"type", "slack", "microsoft-teams", "linear", "jira"}},
	"factory.automations":    {doc: "factory", pointer: "/properties/automations", prefix: "automations."},
	"factory.github":         {doc: "factory", pointer: "/properties/github", prefix: "github."},
	"factory.scorerDefaults": {doc: "factory", pointer: "/properties/scorerDefaults", order: []string{"harness", "model", "machine", "secrets", "mcpServers"}},
	"factory.selfImprovement": {doc: "factory", pointer: "/properties/selfImprovement",
		order: []string{"failedRunThreshold", "reviewerType", "reviewerEmails", "analysisModel"}},
	"factory.agentDefaults": {doc: "factory", pointer: "/properties/agentDefaults",
		order: []string{"model", "harness", "machine", "environmentId", "secrets", "mcpServers", "workerHost"}},
	"harness":     {doc: "common", pointer: "/$defs/harness", order: []string{"type", "model", "reasoningLevel", "auth"}},
	"harnessAuth": {doc: "common", pointer: "/$defs/harnessAuth", order: []string{"source", "secretName"}},
	"agent": {doc: "agent", order: []string{
		"description", "agentType", "credentialStrategy", "spawnableBy", "networkPolicy", "snapshot",
		"model", "harness", "machine", "environmentId", "secrets", "mcpServers", "workerHost", "idleTimeoutMinutes",
	}},
	"automation": {doc: "automation", order: []string{
		"enabled", "displayName", "agent", "runAs", "triggers",
		"model", "harness", "machine", "environmentId", "secrets", "mcpServers", "workerHost", "idleTimeoutMinutes",
	}},
	"automation.trigger": {doc: "automation", pointer: "/$defs/trigger", expand: []string{"schedule"}, order: []string{
		"provider", "event", "filter", "schedule", "schedule.cron", "schedule.name", "concurrency",
	}},
	"machine": {doc: "machine", expand: []string{"instanceShape", "platform", "platform.linux", "platform.mac"}, order: []string{
		"description", "setupCommands", "failureSessionRetentionMinutes", "instanceShape", "instanceShape.vcpus", "instanceShape.memoryGb",
		"platform", "platform.os", "platform.arch", "platform.linux", "platform.linux.image",
		"platform.linux.registryCredentialSecretName", "platform.linux.user", "platform.mac", "platform.mac.version",
	}},
	"webhook": {doc: "webhook", order: []string{"authMode", "signatureScheme", "secretName", "deliveryIdHeader", "enabled"}},
	"scorer": {doc: "scorer", expand: []string{"labels"}, order: []string{
		"name", "description", "output", "agents", "labels", "labels[].value", "labels[].description", "labels[].score",
		"passingScore", "samplingRate", "model", "harness", "selfImprovement", "machine", "secrets", "mcpServers",
	}},
	"benchmark_suite": {doc: "benchmark_suite", expand: []string{"configurations", "configurations[].agents"}, order: []string{
		"name", "description", "agent", "configurations", "configurations[].name", "configurations[].agents",
		"configurations[].agents[].agent", "configurations[].agents[].model", "configurations[].agents[].harness",
		"configurations[].machine", "tasks",
	}},
	"benchmark_suite_task": {doc: "benchmark_suite_task", order: []string{
		"title", "prompt", "successCriteria", "sourceRunId", "tags", "trackerSeed", "linearSeed", "startingRepoRefs",
	}},
	"mcpServer":     {doc: "common", pointer: "/$defs/mcpServer", order: []string{"managedId", "command", "args", "env", "url", "headers", "allowWrite"}},
	"networkPolicy": {doc: "common", pointer: "/$defs/networkPolicy", order: []string{"extends", "allowedHosts", "deniedHosts"}},
}

// defLabels replace the constraints of shared definitions with a link to the
// section of the page that explains them.
var defLabels = map[string]string{
	"resourceName":    "a [resource name](#naming-rules)",
	"secretName":      "a [secret name](#secret-names)",
	"secretList":      "[secret names](#secret-names), no duplicates",
	"harness":         "a complete [harness object](#harness-and-model)",
	"harnessOverride": "a sparse [harness object](#harness-and-model)",
	"harnessAuth":     "an [auth object](#harness-and-model)",
	"mcpServers":      "[MCP server objects](#mcp-server-object) by name; the name `factory` is reserved",
	"networkPolicy":   "a [networkPolicy object](#networkpolicy-object)",
	"host":            "host names or literal IP addresses",
	"trigger":         "[trigger objects](#automationsnameautomationmd)",
}

const docPath = "docs/definition-reference.md"

func main() {
	file := flag.String("file", docPath, "Markdown file with the generated sections")
	check := flag.Bool("check", false, "fail when a generated section is out of date, and change nothing")
	flag.Parse()
	if err := run(*file, *check); err != nil {
		fmt.Fprintln(os.Stderr, "definition-docs:", err)
		os.Exit(1)
	}
}

func run(file string, check bool) error {
	schemas, err := loadSchemas()
	if err != nil {
		return err
	}
	old, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	out, err := render(old, schemas)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if bytes.Equal(old, out) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is out of date with the JSON Schemas; run `make docs-gen`", file)
	}
	if err := os.WriteFile(file, out, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", file)
	return nil
}

func loadSchemas() (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for _, n := range definition.SchemaNames {
		raw, ok := definition.Schema(n)
		if !ok {
			return nil, fmt.Errorf("schema %s not found", n)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("schema %s: %w", n, err)
		}
		out[n] = m
	}
	return out, nil
}

var beginRe = regexp.MustCompile(`^<!-- BEGIN GENERATED: (\S+) \(make docs-gen\) -->$`)

// render replaces the content of every generated section of md.
func render(md []byte, schemas map[string]map[string]any) ([]byte, error) {
	lines := strings.SplitAfter(string(md), "\n")
	var b strings.Builder
	used := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		b.WriteString(lines[i])
		m := beginRe.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n"))
		if m == nil {
			continue
		}
		id := m[1]
		sec, ok := sections[id]
		if !ok {
			return nil, fmt.Errorf("line %d: unknown generated section %q", i+1, id)
		}
		if used[id] {
			return nil, fmt.Errorf("line %d: generated section %q occurs twice", i+1, id)
		}
		used[id] = true
		end := "<!-- END GENERATED: " + id + " -->"
		j := i + 1
		for j < len(lines) && strings.TrimRight(lines[j], "\r\n") != end {
			if beginRe.MatchString(strings.TrimRight(lines[j], "\r\n")) {
				return nil, fmt.Errorf("line %d: section %q has no end marker", i+1, id)
			}
			j++
		}
		if j == len(lines) {
			return nil, fmt.Errorf("line %d: section %q has no end marker %q", i+1, id, end)
		}
		table, err := (&generator{schemas: schemas}).table(sec)
		if err != nil {
			return nil, fmt.Errorf("section %q: %w", id, err)
		}
		b.WriteString(table)
		b.WriteString(lines[j])
		i = j
	}
	var missing []string
	for id := range sections {
		if !used[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("no markers for the sections %s", strings.Join(missing, ", "))
	}
	return []byte(b.String()), nil
}

type generator struct {
	schemas map[string]map[string]any
}

// node is a schema in its document, so that local references resolve.
type node struct {
	doc string
	s   map[string]any
}

type row struct {
	field, typ, required, def, allowed, desc string
}

func (g *generator) table(sec section) (string, error) {
	root, ok := g.schemas[sec.doc]
	if !ok {
		return "", fmt.Errorf("unknown schema %q", sec.doc)
	}
	n, err := g.pointer(node{sec.doc, root}, sec.pointer)
	if err != nil {
		return "", err
	}
	n = g.unwrap(n)
	expand := map[string]bool{}
	for _, e := range sec.expand {
		expand[e] = true
	}
	order := map[string]int{}
	for i, o := range sec.order {
		order[o] = i
	}
	seen := map[string]bool{}
	var rows []row
	if err := g.rows(n, "", sec, expand, order, seen, &rows); err != nil {
		return "", err
	}
	for _, e := range sec.expand {
		if !seen[e] {
			return "", fmt.Errorf("expand %q names no object field", e)
		}
	}
	for _, o := range sec.order {
		if !seen[o] {
			return "", fmt.Errorf("order lists %q, which is not a field", o)
		}
	}
	var b strings.Builder
	b.WriteString("| Field | Type | Required | Default | Allowed values | Description |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n",
			r.field, cell(r.typ), cell(r.required), cell(r.def), cell(r.allowed), cell(text(r.desc)))
	}
	return b.String(), nil
}

// rows appends one row per property of the object n, and the rows of the
// expanded fields after their parent.
func (g *generator) rows(n node, path string, sec section, expand map[string]bool, order map[string]int, seen map[string]bool, out *[]row) error {
	props, _ := n.s["properties"].(map[string]any)
	if len(props) == 0 {
		return fmt.Errorf("%q is not an object with properties", path)
	}
	required := map[string]bool{}
	if req, ok := n.s["required"].([]any); ok {
		for _, r := range req {
			required[fmt.Sprint(r)] = true
		}
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	full := func(k string) string { return path + k }
	sort.Slice(names, func(i, j int) bool {
		oi, iok := order[full(names[i])]
		oj, jok := order[full(names[j])]
		switch {
		case iok && jok:
			return oi < oj
		case iok != jok:
			return iok
		}
		return names[i] < names[j]
	})
	for _, k := range names {
		p := node{n.doc, props[k].(map[string]any)}
		name := full(k)
		seen[name] = true
		desc, _ := p.s["description"].(string)
		if desc == "" {
			return fmt.Errorf("field %q has no description in the schema", name)
		}
		r := row{
			field: sec.prefix + name,
			typ:   g.typeOf(p),
			def:   defaultOf(p.s),
			desc:  desc,
		}
		if required[k] {
			r.required = "yes"
		}
		r.allowed = g.allowed(p)
		*out = append(*out, r)
		if !expand[name] {
			continue
		}
		child := g.unwrap(g.resolve(p))
		childPath := name + "."
		if child.s["type"] == "array" {
			items, ok := child.s["items"].(map[string]any)
			if !ok {
				return fmt.Errorf("%q has no items schema", name)
			}
			child = g.unwrap(g.resolve(node{child.doc, items}))
			childPath = name + "[]."
		}
		if err := g.rows(child, childPath, sec, expand, order, seen, out); err != nil {
			return err
		}
	}
	return nil
}

// pointer follows a JSON pointer from n.
func (g *generator) pointer(n node, ptr string) (node, error) {
	if ptr == "" {
		return n, nil
	}
	var cur any = n.s
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			cur = c[tok]
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return node{}, fmt.Errorf("pointer %s: bad index %q", ptr, tok)
			}
			cur = c[i]
		default:
			cur = nil
		}
		if cur == nil {
			return node{}, fmt.Errorf("pointer %s of %s does not resolve", ptr, n.doc)
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return node{}, fmt.Errorf("pointer %s of %s is not a schema", ptr, n.doc)
	}
	return node{n.doc, m}, nil
}

// refName returns the name of the definition that n references, or "".
func refName(n node) string {
	ref, _ := n.s["$ref"].(string)
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ""
}

// resolve follows $ref until n is not a reference.
func (g *generator) resolve(n node) node {
	for i := 0; i < 10; i++ {
		ref, ok := n.s["$ref"].(string)
		if !ok {
			return n
		}
		doc, ptr, _ := strings.Cut(ref, "#")
		target := n.doc
		if doc != "" {
			target = strings.TrimSuffix(doc, ".schema.json")
		}
		next, err := g.pointer(node{target, g.schemas[target]}, ptr)
		if err != nil {
			return n
		}
		n = next
	}
	return n
}

// branches returns the anyOf or oneOf alternatives of n.
func branches(n node) []node {
	for _, k := range []string{"anyOf", "oneOf"} {
		if list, ok := n.s[k].([]any); ok && n.s["type"] == nil && n.s["properties"] == nil {
			out := make([]node, 0, len(list))
			for _, b := range list {
				if m, ok := b.(map[string]any); ok {
					out = append(out, node{n.doc, m})
				}
			}
			return out
		}
	}
	return nil
}

func isNull(n node) bool { return n.s["type"] == "null" }

// unwrap removes a "null" alternative: {anyOf: [X, {type: null}]} gives X.
func (g *generator) unwrap(n node) node {
	n = g.resolve(n)
	bs := branches(n)
	if len(bs) != 2 {
		return n
	}
	switch {
	case isNull(bs[1]):
		return g.resolve(bs[0])
	case isNull(bs[0]):
		return g.resolve(bs[1])
	}
	return n
}

// typeOf describes the type of n in words.
func (g *generator) typeOf(n node) string {
	n = g.resolve(n)
	if bs := branches(n); len(bs) > 0 {
		var parts []string
		seen := map[string]bool{}
		for _, b := range bs {
			t := g.typeOf(b)
			if !seen[t] {
				seen[t] = true
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, " or ")
	}
	if c, ok := n.s["const"]; ok {
		return jsonType(c)
	}
	switch t := n.s["type"].(type) {
	case string:
		switch t {
		case "array":
			if items, ok := n.s["items"].(map[string]any); ok {
				switch it := g.typeOf(node{n.doc, items}); it {
				case "string", "integer", "number", "boolean", "object":
					return "list of " + it + "s"
				}
			}
			return "list"
		case "object":
			if ap, ok := n.s["additionalProperties"].(map[string]any); ok {
				if it := g.typeOf(node{n.doc, ap}); it == "string" {
					return "map of string to string"
				}
				return "map of objects"
			}
			return "object"
		}
		return t
	case []any:
		var parts []string
		for _, x := range t {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, " or ")
	}
	return "any"
}

func jsonType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	}
	return "any"
}

// allowed describes the values that n accepts.
func (g *generator) allowed(n node) string {
	if name := refName(n); name != "" {
		if l, ok := defLabels[name]; ok {
			return l
		}
		return g.allowed(g.resolve(n))
	}
	if bs := branches(n); len(bs) > 0 {
		// The type column already says "or null". A branch without
		// constraints accepts every value of its type, also in words.
		var parts []string
		for _, b := range bs {
			if isNull(b) {
				continue
			}
			a := g.allowed(b)
			if a == "" {
				a = "any " + g.typeOf(b)
				if g.resolve(b).s["properties"] != nil {
					a = "an object"
				}
			}
			parts = append(parts, a)
		}
		if len(parts) == 1 && strings.HasPrefix(parts[0], "any ") {
			return ""
		}
		return strings.Join(parts, "; or ")
	}
	var parts []string
	if c, ok := n.s["const"]; ok {
		parts = append(parts, literal(c))
	}
	if e, ok := n.s["enum"].([]any); ok {
		vals := make([]string, len(e))
		for i, v := range e {
			vals[i] = literal(v)
		}
		parts = append(parts, strings.Join(vals, ", "))
	}
	if v, ok := n.s["minLength"].(float64); ok && v == 1 {
		if _, hasPattern := n.s["pattern"]; !hasPattern {
			parts = append(parts, "non-empty")
		}
	}
	if p, ok := n.s["pattern"].(string); ok {
		parts = append(parts, "matches "+code(p))
	}
	if r := rangeText(n.s, "minimum", "maximum"); r != "" {
		parts = append(parts, r)
	}
	if v, ok := n.s["exclusiveMinimum"].(float64); ok {
		parts = append(parts, "more than "+num(v))
	}
	switch n.s["type"] {
	case "array":
		if r := countText(n.s); r != "" {
			parts = append(parts, r)
		}
		if u, _ := n.s["uniqueItems"].(bool); u {
			parts = append(parts, "no duplicates")
		}
		if items, ok := n.s["items"].(map[string]any); ok {
			if a := g.allowed(node{n.doc, items}); a != "" {
				parts = append(parts, "each entry: "+a)
			}
		}
	case "object":
		if ap, ok := n.s["additionalProperties"].(map[string]any); ok {
			if a := g.allowed(node{n.doc, ap}); a != "" {
				parts = append(parts, "each value: "+a)
			}
		}
	}
	return strings.Join(parts, "; ")
}

func rangeText(s map[string]any, minKey, maxKey string) string {
	lo, hasMin := s[minKey].(float64)
	hi, hasMax := s[maxKey].(float64)
	switch {
	case hasMin && hasMax:
		return num(lo) + "–" + num(hi)
	case hasMin:
		return num(lo) + " or more"
	case hasMax:
		return "at most " + num(hi)
	}
	return ""
}

func countText(s map[string]any) string {
	lo, hasMin := s["minItems"].(float64)
	hi, hasMax := s["maxItems"].(float64)
	switch {
	case hasMin && hasMax && lo == hi:
		return "exactly " + entries(lo)
	case hasMin && hasMax:
		return num(lo) + "–" + num(hi) + " entries"
	case hasMin:
		return "at least " + entries(lo)
	case hasMax:
		return "at most " + entries(hi)
	}
	return ""
}

func entries(n float64) string {
	if n == 1 {
		return "1 entry"
	}
	return num(n) + " entries"
}

func num(f float64) string {
	if f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func literal(v any) string {
	switch x := v.(type) {
	case string:
		if _, err := strconv.ParseFloat(x, 64); err == nil {
			return code(strconv.Quote(x))
		}
		return code(x)
	case float64:
		return code(num(x))
	case nil:
		return code("null")
	}
	return code(fmt.Sprint(v))
}

// code formats s as a Markdown code span, also when s holds a backtick.
func code(s string) string {
	if strings.Contains(s, "`") {
		return "`` " + s + " ``"
	}
	return "`" + s + "`"
}

func defaultOf(s map[string]any) string {
	v, ok := s["default"]
	if !ok {
		return ""
	}
	return literal(v)
}

// text escapes the plain text of a schema description for Markdown, so that
// a placeholder such as <name> is not read as an HTML tag.
func text(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// cell makes text safe for one cell of a Markdown table.
func cell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", `\|`)
}
