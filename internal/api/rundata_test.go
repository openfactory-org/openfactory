package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/objectstore"
)

// fakeRunData keeps objects in memory. Artifacts cannot be presigned (the
// API streams them); attachments can.
type fakeRunData struct {
	artifacts   map[string]map[string]string
	attachments map[string]map[string]string
}

func objectInfos(m map[string]string) []objectstore.ObjectInfo {
	var out []objectstore.ObjectInfo
	for k, v := range m {
		out = append(out, objectstore.ObjectInfo{Key: k, Size: int64(len(v)), ContentType: "text/plain", LastModified: time.Unix(0, 0)})
	}
	return out
}

func (f *fakeRunData) ListArtifacts(_ context.Context, runID string) ([]objectstore.ObjectInfo, error) {
	return objectInfos(f.artifacts[runID]), nil
}

func (f *fakeRunData) ArtifactURL(context.Context, string, string, time.Duration) (string, error) {
	return "", objectstore.ErrPresignUnsupported
}

func (f *fakeRunData) OpenArtifact(_ context.Context, runID, name string) (io.ReadCloser, objectstore.ObjectInfo, error) {
	v, ok := f.artifacts[runID][name]
	if !ok {
		return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader([]byte(v))), objectstore.ObjectInfo{ContentType: "text/plain"}, nil
}

func (f *fakeRunData) ListAttachments(_ context.Context, runID string) ([]objectstore.ObjectInfo, error) {
	return objectInfos(f.attachments[runID]), nil
}

func (f *fakeRunData) AttachmentURL(_ context.Context, runID, name string, _ time.Duration) (string, error) {
	return "https://objects.example.test/" + runID + "/" + name, nil
}

func (f *fakeRunData) OpenAttachment(context.Context, string, string) (io.ReadCloser, objectstore.ObjectInfo, error) {
	return nil, objectstore.ObjectInfo{}, objectstore.ErrNotFound
}

func (f *fakeRunData) AttachmentUploadURL(_ context.Context, runID, name string, _ time.Duration, _ string) (string, error) {
	return "https://objects.example.test/upload/" + runID + "/" + name, nil
}

func TestRunDataEndpoints(t *testing.T) {
	rd := &fakeRunData{artifacts: map[string]map[string]string{}, attachments: map[string]map[string]string{}}
	e := newTestEnv(t, func(o *Options) { o.RunData = rd })
	run := &core.Run{ID: core.NewID("run"), TeamID: e.team.ID, FactoryID: e.runFactory(), State: core.RunSucceeded, CreatedAt: time.Now().UTC()}
	if err := e.store.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	base := "/runs/" + run.ID

	// A run without messages has an empty transcript.
	if tr := e.must(e.do("GET", base+"/transcript", e.viewer.Key, nil), 200); len(tr.Raw) != 0 {
		t.Fatalf("empty transcript = %q", tr.Raw)
	}
	e.must(e.do("GET", "/runs/run_missing/transcript", e.viewer.Key, nil), 404)
	for _, c := range []string{"one", "two"} {
		if err := e.store.AppendRunMessage(t.Context(), &core.RunMessage{RunID: run.ID, Role: "user", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	tr := e.must(e.do("GET", base+"/transcript", e.viewer.Key, nil), 200)
	if ct := tr.Header.Get("Content-Type"); ct != "application/x-ndjson" || strings.Count(string(tr.Raw), "\n") != 2 {
		t.Fatalf("transcript = %q (%s)", tr.Raw, ct)
	}

	rd.artifacts[run.ID] = map[string]string{"report.txt": "hello"}
	arts := e.must(e.do("GET", base+"/artifacts", e.viewer.Key, nil), 200)
	if items := arts.Body["artifacts"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "report.txt" {
		t.Fatalf("artifacts = %s", arts.Raw)
	}
	if got := e.must(e.do("GET", base+"/artifacts/report.txt", e.viewer.Key, nil), 200); string(got.Raw) != "hello" {
		t.Fatalf("artifact body = %q", got.Raw)
	}
	e.must(e.do("GET", base+"/artifacts/missing.txt", e.viewer.Key, nil), 404)

	rd.attachments[run.ID] = map[string]string{"spec.md": "x"}
	atts := e.must(e.do("GET", base+"/attachments", e.viewer.Key, nil), 200)
	if items := atts.Body["attachments"].([]any); len(items) != 1 || items[0].(map[string]any)["url"] == "" {
		t.Fatalf("attachments = %s", atts.Raw)
	}
	up := e.must(e.do("POST", base+"/attachments", e.viewer.Key, map[string]string{"name": "log.txt", "content_type": "text/plain"}), 201)
	if !strings.HasSuffix(up.Body["upload_url"].(string), "/"+run.ID+"/log.txt") || up.Body["method"] != "PUT" {
		t.Fatalf("upload = %s", up.Raw)
	}
	e.must(e.do("POST", base+"/attachments", e.viewer.Key, map[string]string{"name": "../escape"}), 400)

	// Other teams cannot see the run's data; servers without storage report 501.
	e.must(e.do("GET", base+"/transcript", e.other.Key, nil), 404)
	e2 := newTestEnv(t)
	if r := e2.do("GET", base+"/artifacts", e2.viewer.Key, nil); errCode(r) != CodeFeatureNotAvailable {
		t.Fatalf("without storage = %d %s", r.Status, r.Raw)
	}
}

// The transcript is the list of stored run messages, as JSON Lines, in order.
// It does not need object storage.
func TestRunTranscriptFromRunMessages(t *testing.T) {
	e := newTestEnv(t) // no run data storage
	ctx := t.Context()
	run := &core.Run{ID: core.NewID("run"), TeamID: e.team.ID, FactoryID: e.runFactory(), State: core.RunSucceeded, CreatedAt: time.Now().UTC()}
	if err := e.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	// The runner redacts events before it sends them, so the stored messages hold ***.
	want := []string{"start ***", "tool result ***", "done"}
	for i, c := range want {
		role := []string{"user", "tool_result", "assistant"}[i]
		if err := e.store.AppendRunMessage(ctx, &core.RunMessage{RunID: run.ID, Role: role, Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	lines := func(r resp) []core.RunMessage {
		var out []core.RunMessage
		for _, l := range strings.Split(strings.TrimSuffix(string(r.Raw), "\n"), "\n") {
			var m core.RunMessage
			if err := json.Unmarshal([]byte(l), &m); err != nil {
				t.Fatalf("line %q: %v", l, err)
			}
			out = append(out, m)
		}
		return out
	}
	tr := e.must(e.do("GET", "/runs/"+run.ID+"/transcript", e.viewer.Key, nil), 200)
	if ct := tr.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type = %q", ct)
	}
	got := lines(tr)
	if len(got) != len(want) {
		t.Fatalf("transcript = %s", tr.Raw)
	}
	for i, m := range got {
		if m.Content != want[i] || m.Seq != int64(i+1) || m.RunID != run.ID {
			t.Errorf("message %d = %+v", i, m)
		}
	}
	e.must(e.do("GET", "/runs/"+run.ID+"/transcript", e.other.Key, nil), 404)
	e.must(e.do("GET", "/runs/"+run.ID+"/transcript", "", nil), 401)
}

func TestNestedArtifactNames(t *testing.T) {
	rd := &fakeRunData{artifacts: map[string]map[string]string{}, attachments: map[string]map[string]string{}}
	e := newTestEnv(t, func(o *Options) { o.RunData = rd })
	run := &core.Run{ID: core.NewID("run"), TeamID: e.team.ID, FactoryID: e.runFactory(), State: core.RunSucceeded, CreatedAt: time.Now().UTC()}
	if err := e.store.CreateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	rd.artifacts[run.ID] = map[string]string{"data/out.json": "{}"}
	base := "/runs/" + run.ID + "/artifacts"
	arts := e.must(e.do("GET", base, e.viewer.Key, nil), 200)
	if items := arts.Body["artifacts"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "data/out.json" {
		t.Fatalf("artifacts = %s", arts.Raw)
	}
	if got := e.must(e.do("GET", base+"/data%2Fout.json", e.viewer.Key, nil), 200); string(got.Raw) != "{}" {
		t.Fatalf("nested artifact body = %q", got.Raw)
	}
}
