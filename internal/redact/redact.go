// Package redact removes secret values from text and JSON. The runner and the
// control plane use it, so that a run gives the same result on both sides.
//
// For each secret value the redactor matches the value, its JSON-escaped
// form, its standard and URL-safe base64 forms (with and without padding) and
// its URL-encoded forms. It replaces each match with Mark.
//
// Redaction is a second barrier. A secret that an agent changes in another
// way, for example with hex or by splitting it, passes. Narrow permissions
// and rotation stay necessary.
package redact

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"sync"
)

const (
	// Mark replaces each secret value.
	Mark = "***"
	// MinLen is the length below which New ignores a value, and below which
	// the encoded forms of a value are not made. A shorter value would match
	// too much ordinary text.
	MinLen = 8
)

// Terms lists the strings to remove for values, the longest first, so that a
// secret inside a longer secret leaves no rest. A value shorter than minLen is
// ignored. A value of MinLen or more also gives its encoded forms.
func Terms(values []string, minLen int) []string {
	seen := map[string]bool{}
	var list []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			list = append(list, s)
		}
	}
	for _, v := range values {
		if v == "" || len(v) < minLen {
			continue
		}
		add(v)
		if esc, err := json.Marshal(v); err == nil && len(esc) > 2 {
			add(string(esc[1 : len(esc)-1]))
		}
		if len(v) < MinLen {
			continue
		}
		b := []byte(v)
		add(base64.StdEncoding.EncodeToString(b))
		add(base64.RawStdEncoding.EncodeToString(b))
		add(base64.URLEncoding.EncodeToString(b))
		add(base64.RawURLEncoding.EncodeToString(b))
		add(url.QueryEscape(v))
		add(url.PathEscape(v))
	}
	sort.SliceStable(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	return list
}

// String replaces each term of the list with Mark. The list comes from Terms.
func String(s string, terms []string) string {
	for _, t := range terms {
		s = strings.ReplaceAll(s, t, Mark)
	}
	return s
}

// JSON replaces each term in raw. If the result is not valid JSON, it returns
// {"redacted":true}, which is safe.
func JSON(raw []byte, terms []string) []byte {
	if len(terms) == 0 {
		return raw
	}
	s := String(string(raw), terms)
	if !json.Valid([]byte(s)) {
		return []byte(`{"redacted":true}`)
	}
	return []byte(s)
}

// Redactor removes a growing set of secret values. It is safe for concurrent
// use. A nil Redactor changes nothing.
type Redactor struct {
	min int

	mu     sync.RWMutex
	values map[string]bool
	terms  []string
}

// New makes a redactor that ignores values of fewer than MinLen characters.
func New(values ...string) *Redactor { return NewWithMin(MinLen, values...) }

// NewWithMin makes a redactor that ignores values of fewer than minLen characters.
func NewWithMin(minLen int, values ...string) *Redactor {
	r := &Redactor{min: minLen, values: map[string]bool{}}
	r.Add(values...)
	return r
}

// Add adds secret values.
func (r *Redactor) Add(values ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, v := range values {
		if v != "" && len(v) >= r.min && !r.values[v] {
			r.values[v] = true
			changed = true
		}
	}
	if !changed {
		return
	}
	all := make([]string, 0, len(r.values))
	for v := range r.values {
		all = append(all, v)
	}
	r.terms = Terms(all, r.min)
}

// Len returns the number of secret values.
func (r *Redactor) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.values)
}

func (r *Redactor) list() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.terms
}

// String removes the secret values from s.
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}
	return String(s, r.list())
}

// JSON removes the secret values from a JSON document.
func (r *Redactor) JSON(raw []byte) []byte { return JSON(raw, r.list()) }

// Data removes the secret values from the strings of m, also in nested values.
// It returns m itself if the redactor has no value. The result has the same
// keys, unless a key holds a secret: then the key changes too.
func (r *Redactor) Data(m map[string]any) map[string]any {
	terms := r.list()
	if len(terms) == 0 || len(m) == 0 {
		return m
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return map[string]any{"redacted": true}
	}
	var out map[string]any
	if err := json.Unmarshal(JSON(raw, terms), &out); err != nil {
		return map[string]any{"redacted": true}
	}
	return out
}
