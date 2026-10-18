package triggers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// Payload pattern limits.
const (
	MaxPatternDepth  = 10 // nesting levels of pattern objects (the top level is 1)
	MaxPatternLeaves = 64 // leaf fields across the whole pattern
	MaxPatternValues = 32 // values per in / not_in set
)

// ErrInvalidPattern wraps every payload pattern validation error.
var ErrInvalidPattern = errors.New("invalid payload pattern")

func patternErr(path []string, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if len(path) > 0 {
		msg = strings.Join(path, ".") + ": " + msg
	}
	return fmt.Errorf("%w: %s", ErrInvalidPattern, msg)
}

// patNode is a pattern object: every field must match.
type patNode struct {
	fields []patField
}

type patField struct {
	key    string
	nested *patNode // nested pattern object (requires key presence)
	leaf   *leafMatcher
}

type leafMatcher struct {
	in     []any
	notIn  []any
	exists *bool
}

// ValidatePayloadPattern checks a webhook payload pattern without evaluating
// it. A null or empty pattern is valid and matches every delivery.
//
// Grammar: the pattern is a JSON object. Each key names a payload field and
// maps to one of
//   - an array: the allowed values (shorthand for {"in": [...]});
//   - an operator object whose keys are all among "in", "not_in" and "exists";
//   - any other object: a nested pattern for an object-valued field.
//
// Allowed values are scalars (string, number, boolean, null). Limits: 10
// nesting levels, 64 leaf fields, 32 values per set. A pattern listing the
// same value in both in and not_in on one field, or combining exists:false
// with a non-empty in, is rejected.
func ValidatePayloadPattern(pattern json.RawMessage) error {
	_, err := parsePattern(pattern)
	return err
}

// MatchPayload evaluates a payload pattern against a JSON body. Errors (an
// invalid pattern or a body that is not valid JSON) mean "no match": callers
// must fail closed.
func MatchPayload(pattern json.RawMessage, body []byte) (bool, error) {
	node, err := parsePattern(pattern)
	if err != nil {
		return false, err
	}
	if node == nil || len(node.fields) == 0 {
		return true, nil
	}
	doc, err := decodeJSON(body)
	if err != nil {
		return false, fmt.Errorf("payload is not valid JSON: %w", err)
	}
	return matchNested(node, doc, true), nil
}

func decodeJSON(b []byte) (any, error) {
	if !json.Valid(b) {
		return nil, errors.New("invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func parsePattern(raw json.RawMessage) (*patNode, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil, nil
	}
	v, err := decodeJSON(t)
	if err != nil {
		return nil, patternErr(nil, "not valid JSON")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, patternErr(nil, "pattern must be a JSON object")
	}
	leaves := 0
	return buildNode(obj, nil, 1, &leaves)
}

var operatorKeys = map[string]bool{"in": true, "not_in": true, "exists": true}

func buildNode(obj map[string]any, path []string, depth int, leaves *int) (*patNode, error) {
	if depth > MaxPatternDepth {
		return nil, patternErr(path, "pattern nests deeper than %d levels", MaxPatternDepth)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	n := &patNode{}
	for _, k := range keys {
		p := append(append([]string(nil), path...), k)
		f := patField{key: k}
		switch v := obj[k].(type) {
		case []any:
			lm, err := buildLeaf(map[string]any{"in": v}, p)
			if err != nil {
				return nil, err
			}
			f.leaf = lm
		case map[string]any:
			if isOperatorObject(v) {
				lm, err := buildLeaf(v, p)
				if err != nil {
					return nil, err
				}
				f.leaf = lm
			} else {
				if hasOperatorKey(v) {
					return nil, patternErr(p, "object mixes operators (in, not_in, exists) with field names")
				}
				child, err := buildNode(v, p, depth+1, leaves)
				if err != nil {
					return nil, err
				}
				f.nested = child
				if len(v) == 0 {
					*leaves++ // an empty nested object is a presence check
				}
			}
		default:
			return nil, patternErr(p, "expected an array of allowed values, an operator object or a nested object, got %s", jsonKind(v))
		}
		if f.leaf != nil {
			*leaves++
		}
		if *leaves > MaxPatternLeaves {
			return nil, patternErr(p, "pattern holds more than %d leaf fields", MaxPatternLeaves)
		}
		n.fields = append(n.fields, f)
	}
	return n, nil
}

func isOperatorObject(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	for k := range m {
		if !operatorKeys[k] {
			return false
		}
	}
	return true
}

func hasOperatorKey(m map[string]any) bool {
	for k := range m {
		if operatorKeys[k] {
			return true
		}
	}
	return false
}

func buildLeaf(ops map[string]any, path []string) (*leafMatcher, error) {
	lm := &leafMatcher{}
	var err error
	if v, ok := ops["in"]; ok {
		if lm.in, err = valueSet(v, path, "in"); err != nil {
			return nil, err
		}
	}
	if v, ok := ops["not_in"]; ok {
		if lm.notIn, err = valueSet(v, path, "not_in"); err != nil {
			return nil, err
		}
	}
	if v, ok := ops["exists"]; ok && v != nil {
		b, isBool := v.(bool)
		if !isBool {
			return nil, patternErr(path, "exists must be true or false")
		}
		lm.exists = &b
	}
	for _, a := range lm.in {
		for _, b := range lm.notIn {
			if scalarEqual(a, b) {
				return nil, patternErr(path, "value %s appears in both in and not_in", scalarText(a))
			}
		}
	}
	if lm.exists != nil && !*lm.exists && len(lm.in) > 0 {
		return nil, patternErr(path, "exists: false cannot be combined with a non-empty in")
	}
	return lm, nil
}

func valueSet(v any, path []string, op string) ([]any, error) {
	if v == nil {
		return nil, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, patternErr(path, "%s must be an array of values", op)
	}
	if len(arr) > MaxPatternValues {
		return nil, patternErr(path, "%s lists more than %d values", op, MaxPatternValues)
	}
	out := make([]any, 0, len(arr))
	for _, e := range arr {
		switch e.(type) {
		case nil, string, bool, json.Number:
		default:
			return nil, patternErr(path, "%s values must be strings, numbers, booleans or null, got %s", op, jsonKind(e))
		}
		dup := false
		for _, x := range out {
			if scalarEqual(x, e) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, e)
		}
	}
	return out, nil
}

// matchNested matches a nested pattern against a value: objects recurse,
// arrays match when any object element matches, anything else fails.
func matchNested(n *patNode, v any, present bool) bool {
	if !present {
		return false
	}
	switch x := v.(type) {
	case map[string]any:
		return matchObject(n, x)
	case []any:
		for _, e := range x {
			if m, ok := e.(map[string]any); ok && matchObject(n, m) {
				return true
			}
		}
	}
	return false
}

func matchObject(n *patNode, obj map[string]any) bool {
	for _, f := range n.fields {
		v, present := obj[f.key]
		if f.nested != nil {
			if !matchNested(f.nested, v, present) {
				return false
			}
			continue
		}
		if !f.leaf.match(v, present) {
			return false
		}
	}
	return true
}

func (l *leafMatcher) match(v any, present bool) bool {
	if l.exists != nil && *l.exists != present {
		return false
	}
	if len(l.in) > 0 {
		if !present || !overlaps(v, l.in) {
			return false
		}
	}
	if len(l.notIn) > 0 && present && overlaps(v, l.notIn) {
		return false
	}
	return true
}

// overlaps reports whether v (a scalar, or an array of scalars) equals any
// value in set. Objects never match.
func overlaps(v any, set []any) bool {
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if overlaps(e, set) {
				return true
			}
		}
		return false
	}
	for _, s := range set {
		if scalarEqual(v, s) {
			return true
		}
	}
	return false
}

// scalarEqual compares two decoded JSON scalars by type: strings exactly,
// numbers numerically (1 == 1.0 == 1e0), booleans, and null.
func scalarEqual(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numbersEqual(x.String(), y.String())
	}
	return false
}

func numbersEqual(a, b string) bool {
	if a == b {
		return true
	}
	ra, okA := parseRat(a)
	rb, okB := parseRat(b)
	if okA && okB {
		return ra.Cmp(rb) == 0
	}
	// Extreme exponents: fall back to float comparison.
	fa, _, errA := big.ParseFloat(a, 10, 256, big.ToNearestEven)
	fb, _, errB := big.ParseFloat(b, 10, 256, big.ToNearestEven)
	return errA == nil && errB == nil && fa.Cmp(fb) == 0
}

// parseRat parses a JSON number exactly unless its exponent is huge.
func parseRat(s string) (*big.Rat, bool) {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp := strings.TrimLeft(s[i+1:], "+-")
		if len(exp) > 4 { // |exponent| >= 10000
			return nil, false
		}
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

func scalarText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
