package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func TestRedactJSON(t *testing.T) {
	quote := `a"b\c`
	esc, _ := json.Marshal(quote)
	cases := []struct {
		name    string
		in      string
		secrets []string
		want    string
	}{
		{"plain", `{"content":"token=s3cret"}`, []string{"s3cret"}, `{"content":"token=***"}`},
		{"none", `{"a":"b"}`, nil, `{"a":"b"}`},
		{"empty secret", `{"a":"b"}`, []string{""}, `{"a":"b"}`},
		{"escaped", `{"content":` + string(esc) + `}`, []string{quote}, `{"content":"***"}`},
		{"twice", `{"a":"xx yy xx"}`, []string{"xx"}, `{"a":"*** yy ***"}`},
		{"longest first", `{"a":"abcd"}`, []string{"abc", "abcd"}, `{"a":"***"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(redactJSON([]byte(c.in), c.secrets))
			if got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
			if !json.Valid([]byte(got)) {
				t.Fatalf("not valid JSON: %s", got)
			}
		})
	}
	// A secret that breaks the JSON syntax gives a safe result.
	got := redactJSON([]byte(`{"a":"b"}`), []string{`"a"`})
	if !json.Valid(got) || strings.Contains(string(got), `"a"`) {
		t.Fatalf("got %s", got)
	}
}

func TestCheckRelPath(t *testing.T) {
	bad := []string{"", "/etc/passwd", "..", "../x", "a/../b", `a\..\b`, `\x`}
	for _, p := range bad {
		if err := checkRelPath(p); err == nil {
			t.Errorf("checkRelPath(%q): want an error", p)
		}
	}
	for _, p := range []string{"api", "a/b", "SKILL.md", "a..b", ".openfactory"} {
		if err := checkRelPath(p); err != nil {
			t.Errorf("checkRelPath(%q): %v", p, err)
		}
	}
}

func TestRenderInput(t *testing.T) {
	items := []runnerproto.InputItem{
		{Kind: runnerproto.KindUser, Content: "one"},
		{Kind: runnerproto.KindAgent, Sender: runnerproto.Sender{Type: "agent", Name: "reviewer"}, Content: "two"},
	}
	want := "one\n\n[agent from reviewer]\ntwo"
	if got := RenderInput(items); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEndOfTurn(t *testing.T) {
	if d := endOfTurn(TurnResult{Outcome: runnerproto.OutcomeDone}, true); d.Outcome != runnerproto.OutcomeInterrupted {
		t.Fatalf("interrupt: %+v", d)
	}
	if d := endOfTurn(TurnResult{Outcome: "weird"}, false); d.Outcome != runnerproto.OutcomeError || d.FailureCode != "agent_failed" {
		t.Fatalf("invalid outcome: %+v", d)
	}
	if d := endOfTurn(TurnResult{Outcome: runnerproto.OutcomeError}, false); d.FailureCode != "agent_failed" {
		t.Fatalf("empty failure code: %+v", d)
	}
}

func TestConfigRejectsStateDirInsideWorkDir(t *testing.T) {
	dir := t.TempDir()
	c := Config{ControlURL: "http://x", BootstrapToken: "t", WorkDir: dir, StateDir: dir + "/state"}
	if err := c.prepare(); err == nil {
		t.Fatal("want an error")
	}
}
