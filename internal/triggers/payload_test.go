package triggers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMatchPayload(t *testing.T) {
	body := `{
		"event": "deploy.finished", "env": "production", "service": "payments",
		"count": 1, "ratio": 0.5, "big": 12345678901234567890, "ok": true, "nothing": null,
		"tags": ["a", "b"], "nums": [1, 2, 3],
		"pull_request": {"draft": false, "user": {"login": "alex", "type": "User"}},
		"commits": [{"id": "c1", "author": {"name": "x"}}, {"id": "c2", "author": {"name": "y"}}],
		"labels": [{"name": "bug"}, {"name": "ui"}],
		"Case": "Value"
	}`
	cases := []struct {
		name    string
		pattern string
		want    bool
	}{
		{"null pattern", `null`, true},
		{"empty pattern", `{}`, true},
		{"string in", `{"event": ["deploy.finished"]}`, true},
		{"string in any-of", `{"env": ["staging", "production"]}`, true},
		{"string case-sensitive", `{"Case": ["value"]}`, false},
		{"string exact", `{"Case": ["Value"]}`, true},
		{"not_in passes", `{"env": {"not_in": ["dev", "staging"]}}`, true},
		{"not_in fails", `{"env": {"not_in": ["production"]}}`, false},
		{"missing key fails in", `{"region": ["eu"]}`, false},
		{"missing key passes not_in", `{"region": {"not_in": ["dev"]}}`, true},
		{"exists true", `{"service": {"exists": true}}`, true},
		{"exists true missing", `{"region": {"exists": true}}`, false},
		{"exists false missing", `{"region": {"exists": false}}`, true},
		{"exists false present", `{"service": {"exists": false}}`, false},
		{"null value present for exists", `{"nothing": {"exists": true}}`, true},
		{"null matchable", `{"nothing": [null]}`, true},
		{"null not equal false", `{"nothing": [false]}`, false},
		{"missing not equal null", `{"absent": [null]}`, false},
		{"numbers numerically 1 == 1.0", `{"count": [1.0]}`, true},
		{"numbers 1e0", `{"count": [1e0]}`, true},
		{"number 0.5 == 5e-1", `{"ratio": [5e-1]}`, true},
		{"number vs string", `{"count": ["1"]}`, false},
		{"big integer exact", `{"big": [12345678901234567890]}`, true},
		{"big integer differs", `{"big": [12345678901234567891]}`, false},
		{"bool", `{"ok": [true]}`, true},
		{"bool vs string", `{"ok": ["true"]}`, false},
		{"combined in + exists", `{"env": {"in": ["production"], "exists": true}}`, true},
		{"combined in + not_in", `{"env": {"in": ["production", "dev"], "not_in": ["staging"]}}`, true},
		{"combined fails one", `{"env": {"in": ["production"], "not_in": []}, "event": {"not_in": ["deploy.finished"]}}`, false},
		{"nested object", `{"pull_request": {"draft": [false], "user": {"login": ["alex"]}}}`, true},
		{"nested object mismatch", `{"pull_request": {"user": {"type": {"not_in": ["User"]}}}}`, false},
		{"nested requires presence", `{"repository": {"name": {"not_in": ["x"]}}}`, false},
		{"nested empty object requires presence", `{"pull_request": {}}`, true},
		{"nested empty object missing", `{"repository": {}}`, false},
		{"nested on scalar fails", `{"env": {"name": ["x"]}}`, false},
		{"array of objects any element", `{"commits": {"id": ["c2"]}}`, true},
		{"array of objects deep", `{"commits": {"author": {"name": ["y"]}}}`, true},
		{"array of objects same element", `{"commits": {"id": ["c1"], "author": {"name": ["y"]}}}`, false},
		{"array of objects none", `{"labels": {"name": ["feature"]}}`, false},
		{"array of scalars overlap", `{"tags": ["b", "z"]}`, true},
		{"array of scalars no overlap", `{"tags": ["z"]}`, false},
		{"array of scalars not_in none", `{"tags": {"not_in": ["z"]}}`, true},
		{"array of scalars not_in overlap", `{"tags": {"not_in": ["a"]}}`, false},
		{"array of numbers", `{"nums": [3.0]}`, true},
		{"scalar leaf on object value", `{"pull_request": ["x"]}`, false},
		{"empty in no restriction", `{"env": []}`, true},
		{"empty in on missing", `{"absent": {"in": []}}`, true},
		{"exists false with empty in", `{"absent": {"exists": false, "in": []}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MatchPayload(json.RawMessage(tc.pattern), []byte(body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("MatchPayload = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchPayloadNonObjectBodies(t *testing.T) {
	cases := []struct {
		pattern, body string
		want          bool
	}{
		{`{"a": [1]}`, `[{"a": 2}, {"a": 1}]`, true},
		{`{"a": [1]}`, `[{"a": 2}]`, false},
		{`{"a": [1]}`, `"scalar"`, false},
		{`{}`, `42`, true},
		{`{"a": {"not_in": [1]}}`, `null`, false},
	}
	for _, c := range cases {
		got, err := MatchPayload(json.RawMessage(c.pattern), []byte(c.body))
		if err != nil || got != c.want {
			t.Errorf("MatchPayload(%s, %s) = %v, %v; want %v", c.pattern, c.body, got, err, c.want)
		}
	}
}

func TestMatchPayloadFailsClosed(t *testing.T) {
	if ok, err := MatchPayload(json.RawMessage(`{"a": ["x"]}`), []byte(`{not json`)); ok || err == nil {
		t.Fatalf("invalid body: got %v, %v", ok, err)
	}
	if ok, err := MatchPayload(json.RawMessage(`{"a": "x"}`), []byte(`{"a": "x"}`)); ok || err == nil {
		t.Fatalf("invalid pattern: got %v, %v", ok, err)
	}
}

func deepPattern(levels int) string {
	s := `{"leaf": [1]}`
	for i := 1; i < levels; i++ {
		s = fmt.Sprintf(`{"n%d": %s}`, i, s)
	}
	return s
}

func manyLeaves(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`"k%d": [%d]`, i, i)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func manyValues(n int) string {
	vals := make([]string, n)
	for i := range vals {
		vals[i] = fmt.Sprint(i)
	}
	return `{"k": {"not_in": [` + strings.Join(vals, ",") + `]}}`
}

func TestValidatePayloadPattern(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		errSub  string
	}{
		{"valid doc example", `{"event": ["deploy.finished"], "env": {"not_in": ["dev", "staging"]}, "service": {"exists": true}}`, ""},
		{"null", `null`, ""},
		{"empty", ``, ""},
		{"not object", `["a"]`, "must be a JSON object"},
		{"invalid json", `{"a": [}`, "not valid JSON"},
		{"scalar field", `{"a": "x"}`, "expected an array"},
		{"same value in and not_in", `{"a": {"in": ["x", "y"], "not_in": ["y"]}}`, "both in and not_in"},
		{"same number different spelling", `{"a": {"in": [1], "not_in": [1.0]}}`, "both in and not_in"},
		{"exists false with in", `{"a": {"exists": false, "in": ["x"]}}`, "exists: false"},
		{"exists true with in ok", `{"a": {"exists": true, "in": ["x"]}}`, ""},
		{"exists false with not_in ok", `{"a": {"exists": false, "not_in": ["x"]}}`, ""},
		{"exists non-bool", `{"a": {"exists": "yes"}}`, "true or false"},
		{"in not array", `{"a": {"in": "x"}}`, "must be an array"},
		{"object value in set", `{"a": [{"b": 1}]}`, "values must be"},
		{"array value in set", `{"a": {"in": [[1]]}}`, "values must be"},
		{"mixed operators and fields", `{"a": {"in": ["x"], "b": ["y"]}}`, "mixes operators"},
		{"depth 10 ok", deepPattern(10), ""},
		{"depth 11 rejected", deepPattern(11), "deeper than 10"},
		{"64 leaves ok", manyLeaves(64), ""},
		{"65 leaves rejected", manyLeaves(65), "more than 64 leaf"},
		{"32 values ok", manyValues(32), ""},
		{"33 values rejected", manyValues(33), "more than 32 values"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePayloadPattern(json.RawMessage(tc.pattern))
			if tc.errSub == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want containing %q", err, tc.errSub)
			}
			if !errors.Is(err, ErrInvalidPattern) {
				t.Fatalf("err %v does not wrap ErrInvalidPattern", err)
			}
		})
	}
}

func TestNumbersEqualHugeExponent(t *testing.T) {
	if numbersEqual("1e99999", "2e99999") {
		t.Fatal("different huge numbers compared equal")
	}
	if !numbersEqual("1e99999", "10e99998") {
		t.Fatal("equal huge numbers compared unequal")
	}
}
