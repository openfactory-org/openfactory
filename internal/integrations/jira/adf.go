package jira

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Node is an Atlassian Document Format node.
type Node struct {
	Type    string         `json:"type"`
	Version int            `json:"version,omitempty"`
	Text    string         `json:"text,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Marks   []Mark         `json:"marks,omitempty"`
	Content []Node         `json:"content,omitempty"`
}

// Mark is an ADF text mark.
type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

var (
	inlineRe  = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)|\*\*([^*]+)\*\*|` + "`([^`]+)`")
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	bulletRe  = regexp.MustCompile(`^\s*[-*]\s+(.*)$`)
	orderedRe = regexp.MustCompile(`^\s*\d+[.)]\s+(.*)$`)
)

func inline(s string) []Node {
	var out []Node
	last := 0
	for _, m := range inlineRe.FindAllStringSubmatchIndex(s, -1) {
		if m[0] > last {
			out = append(out, Node{Type: "text", Text: s[last:m[0]]})
		}
		switch {
		case m[2] >= 0:
			out = append(out, Node{Type: "text", Text: s[m[2]:m[3]], Marks: []Mark{{Type: "link", Attrs: map[string]any{"href": s[m[4]:m[5]]}}}})
		case m[6] >= 0:
			out = append(out, Node{Type: "text", Text: s[m[6]:m[7]], Marks: []Mark{{Type: "strong"}}})
		case m[8] >= 0:
			out = append(out, Node{Type: "text", Text: s[m[8]:m[9]], Marks: []Mark{{Type: "code"}}})
		}
		last = m[1]
	}
	if last < len(s) {
		out = append(out, Node{Type: "text", Text: s[last:]})
	}
	return out
}

// MarkdownToADF converts common Markdown (paragraphs, headings, bullet / ordered
// lists, fenced code, links, bold, inline code) into an ADF document.
func MarkdownToADF(md string) Node {
	doc := Node{Type: "doc", Version: 1}
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var para []string
	var list *Node
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		p := Node{Type: "paragraph"}
		for i, l := range para {
			if i > 0 {
				p.Content = append(p.Content, Node{Type: "hardBreak"})
			}
			p.Content = append(p.Content, inline(l)...)
		}
		doc.Content = append(doc.Content, p)
		para = nil
	}
	flushList := func() {
		if list != nil {
			doc.Content = append(doc.Content, *list)
			list = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		trimmed := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(trimmed, "```"):
			flushPara()
			flushList()
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			cb := Node{Type: "codeBlock", Content: []Node{{Type: "text", Text: strings.Join(code, "\n")}}}
			if lang != "" {
				cb.Attrs = map[string]any{"language": lang}
			}
			if len(code) == 0 {
				cb.Content = nil
			}
			doc.Content = append(doc.Content, cb)
		case trimmed == "":
			flushPara()
			flushList()
		case headingRe.MatchString(trimmed):
			flushPara()
			flushList()
			m := headingRe.FindStringSubmatch(trimmed)
			doc.Content = append(doc.Content, Node{Type: "heading", Attrs: map[string]any{"level": len(m[1])}, Content: inline(m[2])})
		case bulletRe.MatchString(l) || orderedRe.MatchString(l):
			flushPara()
			typ, re := "bulletList", bulletRe
			if !bulletRe.MatchString(l) {
				typ, re = "orderedList", orderedRe
			}
			if list != nil && list.Type != typ {
				flushList()
			}
			if list == nil {
				list = &Node{Type: typ}
			}
			item := re.FindStringSubmatch(l)[1]
			list.Content = append(list.Content, Node{Type: "listItem", Content: []Node{{Type: "paragraph", Content: inline(item)}}})
		default:
			flushList()
			para = append(para, l)
		}
	}
	flushPara()
	flushList()
	if len(doc.Content) == 0 {
		doc.Content = []Node{{Type: "paragraph"}}
	}
	return doc
}

// Text extracts plain text from an ADF document or a plain string body (Jira REST v2 / wiki markup).
// Mentions are rendered as "@Name".
func Text(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var n Node
	if json.Unmarshal(raw, &n) != nil {
		return ""
	}
	var b strings.Builder
	var walk func(n Node)
	walk = func(n Node) {
		switch n.Type {
		case "text":
			b.WriteString(n.Text)
		case "mention":
			if t, _ := n.Attrs["text"].(string); t != "" {
				if !strings.HasPrefix(t, "@") {
					b.WriteString("@")
				}
				b.WriteString(t)
			}
		case "hardBreak":
			b.WriteString("\n")
		}
		for _, c := range n.Content {
			walk(c)
		}
		switch n.Type {
		case "paragraph", "heading", "codeBlock", "listItem", "blockquote":
			b.WriteString("\n")
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

var wikiMention = regexp.MustCompile(`\[~accountid:([A-Za-z0-9:_-]+)\]`)

// Mentions returns account IDs mentioned in an ADF document or wiki-markup body.
func Mentions(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		for _, m := range wikiMention.FindAllStringSubmatch(s, -1) {
			out = append(out, m[1])
		}
		return out
	}
	var n Node
	if json.Unmarshal(raw, &n) != nil {
		return nil
	}
	var walk func(n Node)
	walk = func(n Node) {
		if n.Type == "mention" {
			if id, _ := n.Attrs["id"].(string); id != "" {
				out = append(out, id)
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(n)
	return out
}
