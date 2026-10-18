package forgeutil

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

var (
	fencedCode = regexp.MustCompile("(?s)(```|~~~).*?(```|~~~|$)")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
	quoteLine  = regexp.MustCompile(`(?m)^\s*>.*$`)
	// GitHub/GitLab style handles: @login, @org/team, @app-name, @bot[bot]
	mentionRe = regexp.MustCompile(`(^|[^A-Za-z0-9_.\-/@` + "`" + `])@([A-Za-z0-9](?:[A-Za-z0-9_.\-]*[A-Za-z0-9_])?(?:/[A-Za-z0-9_.\-]+)?(?:\[bot\])?)`)
)

// StripCode removes fenced code blocks, inline code spans and quoted lines
// (mentions inside them must not trigger work).
func StripCode(text string) string {
	text = fencedCode.ReplaceAllString(text, " ")
	text = inlineCode.ReplaceAllString(text, " ")
	text = quoteLine.ReplaceAllString(text, " ")
	return text
}

// ExtractMentions returns the distinct @handles in text (outside code), in order, without the "@".
func ExtractMentions(text string) []string {
	text = StripCode(text)
	var out []string
	seen := map[string]bool{}
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		h := strings.TrimRight(m[2], ".")
		k := strings.ToLower(h)
		if h == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, h)
	}
	return out
}

// HasMention reports whether handle (case-insensitive, optional "@") is mentioned in text outside code.
func HasMention(text, handle string) bool {
	handle = strings.TrimPrefix(handle, "@")
	if handle == "" {
		return false
	}
	for _, m := range ExtractMentions(text) {
		if strings.EqualFold(m, handle) || strings.EqualFold(strings.TrimSuffix(m, "[bot]"), handle) {
			return true
		}
	}
	return false
}

// SplitRepo splits "owner/name" (owner may contain slashes, e.g. GitLab subgroups) at the last slash.
func SplitRepo(full string) (owner, name string) {
	full = strings.Trim(full, "/")
	i := strings.LastIndex(full, "/")
	if i < 0 {
		return "", full
	}
	return full[:i], full[i+1:]
}

// Truncate shortens s to at most n runes, adding an ellipsis when cut.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// FormatOutbound renders an outbound message as markdown for comment-based destinations
// (issues, pull requests, tracker comments) that have no native buttons.
func FormatOutbound(msg core.OutboundMessage) string {
	var b strings.Builder
	switch msg.Kind {
	case "question":
		b.WriteString("**Question from the factory**\n\n")
	case "approval_request":
		b.WriteString("**Approval needed**\n\n")
	case "error":
		b.WriteString("**The factory hit an error**\n\n")
	}
	b.WriteString(strings.TrimSpace(msg.Text))
	if len(msg.Options) > 0 {
		b.WriteString("\n\nReply with one of:\n")
		for _, o := range msg.Options {
			fmt.Fprintf(&b, "- `%s`\n", o)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// StringSet returns the distinct non-empty values of in, preserving order.
func StringSet(in ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// Diff returns the elements of after that are not in before.
func Diff(after, before []string) []string {
	b := map[string]bool{}
	for _, s := range before {
		b[s] = true
	}
	out := []string{}
	for _, s := range after {
		if !b[s] {
			out = append(out, s)
		}
	}
	return out
}
