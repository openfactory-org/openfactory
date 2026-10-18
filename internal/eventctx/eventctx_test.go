package eventctx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func fileMap(b *Bundle) map[string]string {
	out := map[string]string{}
	for _, f := range b.Files {
		out[f.Path] = string(f.Data)
	}
	return out
}

func TestBuildWebhook(t *testing.T) {
	ev := &core.Event{Provider: "webhook", Type: "received", DeliveryID: "dlv-7",
		Actor:      core.Actor{Handle: "ci-alerts", ExternalID: "wh_1"},
		Attributes: map[string]any{"webhook_id": "wh_1", "webhook_name": "ci-alerts"},
		Payload:    json.RawMessage(`{"build":{"status":"failed","id":42}}`)}
	b := Build(context.Background(), "msg_1", ev, true, nil)
	files := fileMap(b)
	if got := files[".openfactory/events/msg_1/payload.json"]; got != `{"build":{"status":"failed","id":42}}` {
		t.Fatalf("payload = %q", got)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(files[".openfactory/events/msg_1/envelope.json"]), &env); err != nil {
		t.Fatal(err)
	}
	if env.Webhook == nil || env.Webhook.Name != "ci-alerts" || env.Webhook.Delivery != "dlv-7" || env.PayloadTruncated {
		t.Fatalf("envelope = %+v", env)
	}
	sec := b.PromptSection()
	for _, want := range []string{"untrusted", "`.openfactory/events/msg_1/`", "Webhook: ci-alerts, delivery dlv-7", "payload.json"} {
		if !strings.Contains(sec, want) {
			t.Errorf("prompt section misses %q:\n%s", want, sec)
		}
	}
}

func TestBuildCutsLargePayload(t *testing.T) {
	big := `"` + strings.Repeat("é", MaxPayloadBytes) + `"`
	b := Build(context.Background(), "m", &core.Event{Provider: "webhook", Payload: json.RawMessage(big)}, false, nil)
	p := fileMap(b)[".openfactory/events/m/payload.json"]
	if len(p) > MaxPayloadBytes || !b.Envelope.PayloadTruncated || b.Envelope.PayloadBytes != len(big) {
		t.Fatalf("len=%d truncated=%v bytes=%d", len(p), b.Envelope.PayloadTruncated, b.Envelope.PayloadBytes)
	}
	if !strings.Contains(b.PromptSection(), "cut to 256 KiB") {
		t.Errorf("section: %s", b.PromptSection())
	}
}

type fakeBuilder struct {
	files   []File
	omitted []Omitted
	err     error
	calls   int
}

func (f *fakeBuilder) Provider() string { return "slack" }
func (f *fakeBuilder) EventContext(context.Context, *core.Event) ([]File, []Omitted, error) {
	f.calls++
	return f.files, f.omitted, f.err
}

func TestBuildContextAndOmitted(t *testing.T) {
	fb := &fakeBuilder{files: []File{{Path: "thread.json", Data: []byte(`[]`)}, {Path: "../../escape", Data: []byte("x")}},
		omitted: []Omitted{{Name: "big.bin", Reason: "larger than 1024 KiB"}}}
	ev := &core.Event{Provider: "slack", Type: "message"}
	b := Build(context.Background(), "m1", ev, true, []Builder{fb})
	files := fileMap(b)
	if files[".openfactory/events/m1/context/thread.json"] != "[]" || files[".openfactory/events/m1/context/escape"] != "x" {
		t.Fatalf("files = %v", files)
	}
	if !strings.Contains(b.PromptSection(), "Left out: big.bin") {
		t.Fatalf("section: %s", b.PromptSection())
	}
	// A later message has no context.
	b2 := Build(context.Background(), "m2", ev, false, []Builder{fb})
	if fb.calls != 1 || len(b2.Envelope.ContextFiles) != 0 {
		t.Fatalf("calls = %d, context = %v", fb.calls, b2.Envelope.ContextFiles)
	}
	// A builder error does not fail the build.
	b3 := Build(context.Background(), "m3", ev, true, []Builder{&fakeBuilder{err: errors.New("no scope")}})
	if len(b3.Envelope.Omitted) != 1 || b3.Envelope.Omitted[0].Reason != "no scope" {
		t.Fatalf("omitted = %+v", b3.Envelope.Omitted)
	}
}

func TestLinearIssueContext(t *testing.T) {
	ev := &core.Event{Provider: "linear", Type: "issue_created", Payload: json.RawMessage(`{"type":"Issue","data":{
		"identifier":"ENG-9","title":"Fix login","description":"Steps...","state":{"name":"Todo"},
		"assignee":{"name":"Dana"},"project":{"name":"Auth"},"labels":[{"name":"bug"}]}}`)}
	b := Build(context.Background(), "m", ev, true, nil)
	var is Issue
	if err := json.Unmarshal([]byte(fileMap(b)[".openfactory/events/m/context/issue.json"]), &is); err != nil {
		t.Fatal(err)
	}
	if is.Key != "ENG-9" || is.State != "Todo" || is.Assignee != "Dana" || is.Project != "Auth" || len(is.Labels) != 1 {
		t.Fatalf("issue = %+v", is)
	}
}

func TestJiraIssueContext(t *testing.T) {
	ev := &core.Event{Provider: "jira", Type: "issue_created", Payload: json.RawMessage(`{"issue":{"key":"OPS-1","fields":{
		"summary":"Timeout","status":{"name":"Open"},"labels":["sev2"],"assignee":{"displayName":"Lee"},"project":{"name":"Ops"},
		"description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"It times out."}]}]}}}}`)}
	b := Build(context.Background(), "m", ev, true, nil)
	var is Issue
	if err := json.Unmarshal([]byte(fileMap(b)[".openfactory/events/m/context/issue.json"]), &is); err != nil {
		t.Fatal(err)
	}
	if is.Key != "OPS-1" || is.Description != "It times out." || is.Assignee != "Lee" || is.State != "Open" {
		t.Fatalf("issue = %+v", is)
	}
}

func TestReviewCommentContext(t *testing.T) {
	ev := &core.Event{Provider: "github", Type: "pull_request_review_comment_created", Payload: json.RawMessage(`{"comment":{
		"body":"Use a constant","path":"api/x.go","diff_hunk":"@@ -1,2 +1,2 @@\n-a\n+b","line":2,"user":{"login":"rev"}}}`)}
	files := fileMap(Build(context.Background(), "m", ev, true, nil))
	if !strings.Contains(files[".openfactory/events/m/context/diff.patch"], "+++ b/api/x.go\n@@ -1,2 +1,2 @@") {
		t.Fatalf("diff = %q", files[".openfactory/events/m/context/diff.patch"])
	}
	if !strings.Contains(files[".openfactory/events/m/context/review_comment.json"], "Use a constant") {
		t.Fatalf("files = %v", files)
	}
}

func TestRedactAndCodec(t *testing.T) {
	b := Build(context.Background(), "m", &core.Event{Provider: "webhook", Payload: json.RawMessage(`{"token":"s3cr3t-value"}`)}, false, nil)
	raw, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil || len(got.Files) != len(b.Files) {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	red := Redact(got.Files, func(s string) string { return strings.ReplaceAll(s, "s3cr3t-value", "[REDACTED]") })
	for _, f := range red {
		if strings.Contains(string(f.Data), "s3cr3t-value") {
			t.Fatalf("secret in %s", f.Path)
		}
	}
	if MessageDir("../x") != ".openfactory/events/___x" {
		t.Fatalf("dir = %s", MessageDir("../x"))
	}
}
