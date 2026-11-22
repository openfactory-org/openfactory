package harness

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// MCPTransport classifies a server entry.
func MCPTransport(s rtcore.MCPServer) string {
	switch {
	case s.Command != "":
		return "stdio"
	case s.URL != "":
		if IsSSEURL(s.URL) {
			return "sse"
		}
		return "http"
	case s.ManagedID != "":
		return "managed"
	}
	return ""
}

// IsSSEURL reports whether a URL looks like a legacy SSE endpoint (path ending in /sse).
func IsSSEURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/sse")
}

// SortedServerNames returns map keys sorted.
func SortedServerNames(servers map[string]rtcore.MCPServer) []string {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ClaudeMCPConfig renders a Claude Code --mcp-config file:
//
//	{"mcpServers": {"<name>": {"type":"stdio","command":...,"args":[...],"env":{...}} |
//	                          {"type":"http"|"sse","url":...,"headers":{...}}}}
//
// Entries that only carry a managedId (not resolved to a transport by the planner)
// are skipped and reported in warnings.
func ClaudeMCPConfig(servers map[string]rtcore.MCPServer) ([]byte, []string) {
	var warnings []string
	out := map[string]any{}
	for _, name := range SortedServerNames(servers) {
		s := servers[name]
		switch MCPTransport(s) {
		case "stdio":
			e := map[string]any{"type": "stdio", "command": s.Command, "args": nonNil(s.Args)}
			if len(s.Env) > 0 {
				e["env"] = s.Env
			}
			out[name] = e
		case "http", "sse":
			e := map[string]any{"type": MCPTransport(s), "url": s.URL}
			if len(s.Headers) > 0 {
				e["headers"] = s.Headers
			}
			out[name] = e
		default:
			warnings = append(warnings, fmt.Sprintf("mcp server %q skipped: no command or url (managedId must be resolved before the run)", name))
		}
	}
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": out}, "", "  ")
	return b, warnings
}

func nonNil(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

// CodexMCPConfigTOML renders the [mcp_servers.*] tables of a Codex config.toml.
func CodexMCPConfigTOML(servers map[string]rtcore.MCPServer) (string, []string) {
	var warnings []string
	var b strings.Builder
	for _, name := range SortedServerNames(servers) {
		s := servers[name]
		key := "mcp_servers." + TOMLKey(name)
		switch MCPTransport(s) {
		case "stdio":
			fmt.Fprintf(&b, "[%s]\ncommand = %s\nargs = %s\nstartup_timeout_sec = 60\ntool_timeout_sec = 600\n", key, TOMLString(s.Command), tomlStringArray(s.Args))
			if len(s.Env) > 0 {
				fmt.Fprintf(&b, "\n[%s.env]\n%s", key, tomlTable(s.Env))
			}
			b.WriteString("\n")
		case "http", "sse":
			fmt.Fprintf(&b, "[%s]\nurl = %s\nstartup_timeout_sec = 60\ntool_timeout_sec = 600\n", key, TOMLString(s.URL))
			if len(s.Headers) > 0 {
				fmt.Fprintf(&b, "\n[%s.http_headers]\n%s", key, tomlTable(s.Headers))
			}
			b.WriteString("\n")
		default:
			warnings = append(warnings, fmt.Sprintf("mcp server %q skipped: no command or url (managedId must be resolved before the run)", name))
		}
	}
	return b.String(), warnings
}

// TOMLString quotes s as a TOML basic string.
func TOMLString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TOMLKey returns k as a bare key when possible, else quoted.
func TOMLKey(k string) string {
	if k == "" {
		return `""`
	}
	for _, r := range k {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return TOMLString(k)
		}
	}
	return k
}

func tomlStringArray(a []string) string {
	parts := make([]string, len(a))
	for i, s := range a {
		parts[i] = TOMLString(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func tomlTable(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", TOMLKey(k), TOMLString(m[k]))
	}
	return b.String()
}

// TOMLValue renders a scalar for a `-c key=value` override.
func TOMLValue(v any) string {
	switch x := v.(type) {
	case string:
		return TOMLString(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	default:
		return TOMLString(fmt.Sprint(v))
	}
}
