package triggers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ErrInvalidFilter wraps every filter validation error.
var ErrInvalidFilter = errors.New("invalid trigger filter")

func filterErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidFilter, fmt.Sprintf(format, args...))
}

// parseMode selects authoring (definition files / API) or matching rules.
type parseMode int

const (
	modeMatch  parseMode = iota // server-managed keys allowed
	modeAuthor                  // server-managed keys and non-authorable events rejected
)

// condition is one parsed filter key.
type condition struct {
	key   string
	spec  fieldSpec
	in    []string // normalized
	notIn []string // normalized
}

// parsedFilter is a validated trigger filter.
type parsedFilter struct {
	conds    []condition
	skipBots string // github only; defaults to "all"
	payload  json.RawMessage
}

// parseFilter validates raw against the provider/event catalog.
func parseFilter(provider, event string, raw json.RawMessage, mode parseMode) (*parsedFilter, error) {
	ps, ok := catalog[provider]
	if !ok {
		return nil, filterErr("unknown provider %q", provider)
	}
	es, ok := ps.events[event]
	if !ok {
		return nil, filterErr("unknown event %q for provider %q", event, provider)
	}
	if mode == modeAuthor && es.notAuthorable {
		return nil, filterErr("event %s.%s is not available for authoring automations", provider, event)
	}
	pf := &parsedFilter{}
	if provider == ProviderGitHub {
		pf.skipBots = SkipBotsAll
	}

	obj := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if trimmed[0] != '{' {
			return nil, filterErr("filter must be an object")
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, filterErr("filter is not valid JSON: %v", err)
		}
	}
	if provider == ProviderSchedule && mode == modeAuthor && len(obj) > 0 {
		return nil, filterErr("schedule triggers accept no filter")
	}

	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	sort.Strings(names) // deterministic error messages

	seenNative := map[string]string{} // canonical key -> key used
	for _, key := range names {
		val := obj[key]
		switch key {
		case keySkipBots:
			if !contains(es.keys, keySkipBots) {
				return nil, filterErr("key %q is not supported for %s.%s", key, provider, event)
			}
			sb, err := parseSkipBots(val)
			if err != nil {
				return nil, err
			}
			if sb != "" {
				pf.skipBots = sb
			}
			continue
		case keyPayload:
			if !contains(es.keys, keyPayload) {
				return nil, filterErr("key %q is not supported for %s.%s", key, provider, event)
			}
			if err := ValidatePayloadPattern(val); err != nil {
				return nil, err
			}
			if !isNullOrEmpty(val) {
				pf.payload = val
			}
			continue
		}

		spec, isField := ps.fields[key]
		canonical := key
		if !isField {
			as, isAlias := ps.aliases[key]
			if !isAlias {
				return nil, filterErr("unknown key %q for %s.%s", key, provider, event)
			}
			spec, canonical = as, as.native
		}
		if !contains(es.keys, canonical) {
			return nil, filterErr("key %q is not supported for %s.%s", key, provider, event)
		}
		if spec.serverManaged && mode == modeAuthor {
			return nil, filterErr("key %q is server-managed and cannot be set", key)
		}
		if prev, dup := seenNative[canonical]; dup {
			return nil, filterErr("keys %q and %q select the same field; use only one", prev, key)
		}
		seenNative[canonical] = key

		cond, err := parseCondition(key, spec, val)
		if err != nil {
			return nil, err
		}
		if len(cond.in) == 0 && len(cond.notIn) == 0 {
			continue // no restriction
		}
		pf.conds = append(pf.conds, cond)
	}

	// Required keys (Teams scope, webhook UID).
	for _, k := range es.keys {
		spec, ok := ps.fields[k]
		if !ok || !spec.required {
			continue
		}
		found := false
		for _, c := range pf.conds {
			if (c.spec.native == "" && c.key == k || c.spec.native == k) && len(c.in) > 0 {
				found = true
			}
		}
		if !found {
			return nil, filterErr("key %q is required for %s.%s", k, provider, event)
		}
	}
	return pf, nil
}

func isNullOrEmpty(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

func parseSkipBots(raw json.RawMessage) (string, error) {
	if isNullOrEmpty(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", filterErr("skip_bots must be one of all, self, none")
	}
	switch s {
	case SkipBotsAll, SkipBotsSelf, SkipBotsNone:
		return s, nil
	}
	return "", filterErr("skip_bots must be one of all, self, none (got %q)", s)
}

func parseCondition(key string, spec fieldSpec, raw json.RawMessage) (condition, error) {
	c := condition{key: key, spec: spec}
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return c, nil
	}
	var inRaw, notInRaw json.RawMessage
	switch t[0] {
	case '[':
		inRaw = t
	case '{':
		if spec.plainListOnly {
			return c, filterErr("key %q accepts a plain list only", key)
		}
		var ops map[string]json.RawMessage
		if err := json.Unmarshal(t, &ops); err != nil {
			return c, filterErr("key %q: %v", key, err)
		}
		for op, v := range ops {
			switch op {
			case "in":
				inRaw = v
			case "not_in":
				notInRaw = v
			default:
				return c, filterErr("key %q: unknown operator %q", key, op)
			}
		}
	default:
		return c, filterErr("key %q must be a list or a matcher object", key)
	}
	var err error
	if c.in, err = parseValues(key, spec, inRaw); err != nil {
		return c, err
	}
	if c.notIn, err = parseValues(key, spec, notInRaw); err != nil {
		return c, err
	}
	if spec.inOnly && len(c.notIn) > 0 {
		return c, filterErr("key %q supports only in", key)
	}
	if spec.exactlyOne && len(c.in) != 1 && (len(c.in) > 0 || spec.required) {
		return c, filterErr("key %q requires exactly one value", key)
	}
	for _, v := range c.in {
		if contains(c.notIn, v) {
			return c, filterErr("key %q lists %q in both in and not_in", key, v)
		}
	}
	return c, nil
}

func parseValues(key string, spec fieldSpec, raw json.RawMessage) ([]string, error) {
	if isNullOrEmpty(raw) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, filterErr("key %q: values must be a list", key)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		v, err := parseValue(key, spec, it)
		if err != nil {
			return nil, err
		}
		if spec.enum != nil && !contains(spec.enum, v) {
			return nil, filterErr("key %q: unsupported value %q (allowed: %s)", key, v, strings.Join(spec.enum, ", "))
		}
		if !contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

func parseValue(key string, spec fieldSpec, raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", filterErr("key %q: %v", key, err)
	}
	if spec.cmp == cmpInt {
		n, ok := v.(json.Number)
		if !ok {
			return "", filterErr("key %q takes integers, not %s", key, jsonKind(v))
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil || i <= 0 {
			return "", filterErr("key %q takes positive integers (got %s)", key, n)
		}
		return strconv.FormatInt(i, 10), nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case json.Number:
		s = x.String()
	default:
		return "", filterErr("key %q takes strings, not %s", key, jsonKind(v))
	}
	s = normalize(spec.cmp, s)
	if s == "" && spec.cmp != cmpNoop {
		return "", filterErr("key %q contains an empty value", key)
	}
	if spec.cmp == cmpGlob {
		if err := validateGlob(s); err != nil {
			return "", filterErr("key %q: %v", key, err)
		}
	}
	return s, nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case []any:
		return "a list"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

// normalize canonicalizes a filter or attribute value for comparison.
func normalize(k cmpKind, s string) string {
	switch k {
	case cmpFold, cmpKeyword:
		return strings.ToLower(strings.TrimSpace(s))
	case cmpEmoji:
		return normalizeEmoji(s)
	case cmpBranch:
		s = strings.TrimSpace(s)
		return strings.TrimPrefix(s, "refs/heads/")
	case cmpChannelName:
		s = strings.TrimSpace(s)
		return strings.TrimSpace(strings.TrimPrefix(s, "#"))
	case cmpChannelNameFold:
		return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#")))
	case cmpGlob:
		return strings.TrimPrefix(strings.TrimSpace(s), "/")
	case cmpNoop:
		return s
	default:
		return strings.TrimSpace(s)
	}
}

// normalizeEmoji strips surrounding colons and skin-tone suffixes:
// ":thumbsup::skin-tone-3:" -> "thumbsup".
func normalizeEmoji(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "::skin-tone-"); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, ":")
	if i := strings.Index(s, ":skin-tone-"); i >= 0 {
		s = s[:i]
	}
	return s
}

// evalCondition reports whether ev satisfies c.
func evalCondition(attrs map[string]any, text string, c condition) (bool, error) {
	if c.spec.cmp == cmpNoop {
		return true, nil
	}
	var values []string
	if c.spec.cmp == cmpKeyword {
		if text != "" {
			values = []string{strings.ToLower(text)}
		}
	} else {
		var err error
		values, _, err = collect(attrs, c.spec.attrs, c.spec.cmp)
		if err != nil {
			return false, err
		}
	}
	if len(c.in) > 0 {
		inValues := values
		if c.spec.addedAttr != "" {
			added, present, err := collect(attrs, []string{c.spec.addedAttr}, c.spec.cmp)
			if err != nil {
				return false, err
			}
			if present {
				inValues = added
			}
		}
		if !anyMatch(c.spec.cmp, c.in, inValues) {
			return false, nil
		}
	}
	if len(c.notIn) > 0 && anyMatch(c.spec.cmp, c.notIn, values) {
		return false, nil
	}
	return true, nil
}

func anyMatch(k cmpKind, patterns, values []string) bool {
	for _, v := range values {
		for _, p := range patterns {
			switch k {
			case cmpGlob:
				if matchGlob(p, v) {
					return true
				}
			case cmpKeyword:
				if strings.Contains(v, p) {
					return true
				}
			default:
				if p == v {
					return true
				}
			}
		}
	}
	return false
}

// collect returns the normalized values of the named attributes (union).
// present reports whether at least one attribute key exists on the event.
func collect(attrs map[string]any, names []string, k cmpKind) (values []string, present bool, err error) {
	for _, n := range names {
		raw, ok := attrs[n]
		if !ok || raw == nil {
			continue
		}
		present = true
		vs, err := toStrings(raw)
		if err != nil {
			return nil, present, fmt.Errorf("attribute %q: %w", n, err)
		}
		for _, v := range vs {
			if k == cmpInt {
				v = canonicalInt(v)
			} else {
				v = normalize(k, v)
			}
			if v != "" {
				values = append(values, v)
			}
		}
	}
	return values, present, nil
}

func canonicalInt(s string) string {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(i, 10)
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) && !math.IsInf(f, 0) {
		return strconv.FormatInt(int64(f), 10)
	}
	return s
}

func toStrings(v any) ([]string, error) {
	switch x := v.(type) {
	case []string:
		return x, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, err := scalarString(e)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case []int:
		out := make([]string, len(x))
		for i, n := range x {
			out[i] = strconv.Itoa(n)
		}
		return out, nil
	default:
		s, err := scalarString(v)
		if err != nil {
			return nil, err
		}
		return []string{s}, nil
	}
}

func scalarString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case int:
		return strconv.Itoa(x), nil
	case int32:
		return strconv.FormatInt(int64(x), 10), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(x), nil
	}
	return "", fmt.Errorf("unsupported attribute value type %T", v)
}

func attrBool(attrs map[string]any, name string) bool {
	switch x := attrs[name].(type) {
	case bool:
		return x
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	}
	return false
}
