package secrets

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/openfactory-org/openfactory/internal/core"
)

// placeholderRe matches {{NAME}} with optional inner whitespace.
var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// Placeholders returns the distinct secret names referenced in s, in order of appearance.
func Placeholders(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// ExpandWith replaces {{NAME}} placeholders using lookup. Every placeholder is
// resolved once; names whose lookup returns core.ErrNotFound are collected and
// reported together.
func ExpandWith(s string, lookup func(name string) (string, error)) (string, error) {
	names := Placeholders(s)
	if len(names) == 0 {
		return s, nil
	}
	values := make(map[string]string, len(names))
	var missing []string
	for _, n := range names {
		v, err := lookup(n)
		if errors.Is(err, core.ErrNotFound) {
			missing = append(missing, n)
			continue
		}
		if err != nil {
			return "", fmt.Errorf("resolve secret %s: %w", n, err)
		}
		values[n] = v
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("%w: unknown secret(s) referenced: %s", core.ErrNotFound, strings.Join(missing, ", "))
	}
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		return values[placeholderRe.FindStringSubmatch(m)[1]]
	}), nil
}

// ExpandMap expands every value of m (e.g. MCP env or headers).
func ExpandMap(m map[string]string, lookup func(name string) (string, error)) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		ev, err := ExpandWith(v, lookup)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = ev
	}
	return out, nil
}

// RedactedMarker replaces secret values in redacted text.
const RedactedMarker = "[REDACTED]"

// minRedactLen avoids redacting trivially short values that would garble text.
const minRedactLen = 4

// Redact replaces every occurrence of each value in text with [REDACTED].
// Longer values are replaced first so overlapping secrets are fully hidden.
func Redact(text string, values []string) string {
	if text == "" || len(values) == 0 {
		return text
	}
	vs := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= minRedactLen {
			vs = append(vs, v)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return len(vs[i]) > len(vs[j]) })
	for _, v := range vs {
		text = strings.ReplaceAll(text, v, RedactedMarker)
	}
	return text
}

// Redactor scrubs a fixed set of secret values; safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

// NewRedactor creates a redactor for values.
func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	r.Add(values...)
	return r
}

// Add registers more values.
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		if len(v) >= minRedactLen {
			r.values = append(r.values, v)
		}
	}
}

// Redact scrubs text.
func (r *Redactor) Redact(text string) string {
	if r == nil {
		return text
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Redact(text, r.values)
}

// Writer returns an io.Writer that redacts each Write before forwarding it.
// Values split across two writes are not detected; wrap line-oriented streams.
func (r *Redactor) Writer(w io.Writer) io.Writer { return redactWriter{r: r, w: w} }

type redactWriter struct {
	r *Redactor
	w io.Writer
}

func (rw redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(rw.w, rw.r.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
