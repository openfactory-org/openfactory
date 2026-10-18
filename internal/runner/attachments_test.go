package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func attachmentRunner(t *testing.T, files map[string]string) (*runner, string) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == runnerproto.PathSessionDownloads {
			id := req.URL.Query().Get("id")
			body, ok := files[id]
			if req.URL.Query().Get("kind") != "attachment" || !ok {
				http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(runnerproto.DownloadsResponse{Files: []runnerproto.DownloadFile{{
				Part: "attachment", Name: id + ".txt", URL: srv.URL + "/obj/" + id, Bytes: int64(len(body)),
			}}})
			return
		}
		if id, ok := strings.CutPrefix(req.URL.Path, "/obj/"); ok {
			_, _ = w.Write([]byte(files[id]))
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)
	cl, err := runnerclient.New(runnerclient.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	cl.SetToken("tok")
	dir := t.TempDir()
	ev, err := runnerclient.OpenEventLog(filepath.Join(dir, "events.jsonl"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ev.Close() })
	work := filepath.Join(dir, "work")
	return &runner{log: quietLog(), client: cl, events: ev, turnDir: work, flushCh: make(chan struct{}, 1)}, work
}

func TestFetchAttachments(t *testing.T) {
	r, work := attachmentRunner(t, map[string]string{"att_1": "hello", "att_2": "world", "att_3": "x"})
	items := []runnerproto.InputItem{{
		MessageID: "msg_1", Kind: runnerproto.KindUser, Content: "see files",
		Attachments: []runnerproto.Attachment{
			{ID: "att_1", Name: "a.txt", ContentType: "text/plain", Bytes: 5},
			{ID: "att_2", Name: "b.txt", ContentType: "text/plain", Bytes: 5, Path: "docs/b.txt"},
			{ID: "att_3", Name: "c.txt", Bytes: 1, Path: "../escape.txt"},
			{ID: "att_missing", Name: "d.txt", Bytes: 1},
		},
	}}
	out := r.fetchAttachments(context.Background(), items)
	atts := out[0].Attachments
	if atts[0].Path != ".openfactory/attachments/msg_1/a.txt" {
		t.Errorf("default path = %q", atts[0].Path)
	}
	if atts[1].Path != "docs/b.txt" {
		t.Errorf("set path = %q", atts[1].Path)
	}
	if atts[2].Path != "" || atts[3].Path != "" {
		t.Errorf("failed paths = %q %q", atts[2].Path, atts[3].Path)
	}
	if items[0].Attachments[0].Path != "" {
		t.Error("the input was changed")
	}
	for rel, want := range map[string]string{atts[0].Path: "hello", atts[1].Path: "world"} {
		got, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v", rel, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(work), "escape.txt")); err == nil {
		t.Error("a file left the work directory")
	}
	evs, err := r.events.After(0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == runnerproto.EventDiagnostic && strings.Contains(string(e.Data), attachmentDiagCode) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d download diagnostics, want 2", n)
	}

	text := RenderInput(out)
	if !strings.Contains(text, "Attachment: .openfactory/attachments/msg_1/a.txt (text/plain, 5 bytes)") ||
		!strings.Contains(text, "Attachment: docs/b.txt (text/plain, 5 bytes)") || strings.Contains(text, "escape") {
		t.Errorf("rendered input:\n%s", text)
	}
}

// TestInputFiles checks the event context on the runner path: the inline files of an item
// (the event context) go to their path in the work directory, with the
// secret values redacted, and do not reach the harness.
func TestInputFiles(t *testing.T) {
	r, work := attachmentRunner(t, nil)
	r.redact = []string{"s3cr3t-token"}
	items := []runnerproto.InputItem{{
		MessageID: "msg_1", Kind: runnerproto.KindUser, Content: "event",
		Files: []runnerproto.InputFile{
			{Path: ".openfactory/events/msg_1/payload.json", Data: []byte(`{"status":"failed","token":"s3cr3t-token"}`)},
			{Path: "../outside.txt", Data: []byte("x")},
		},
	}}
	out := r.fetchAttachments(context.Background(), items)
	if len(out[0].Files) != 0 || len(items[0].Files) != 2 {
		t.Fatalf("files out=%d in=%d", len(out[0].Files), len(items[0].Files))
	}
	b, err := os.ReadFile(filepath.Join(work, ".openfactory/events/msg_1/payload.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "s3cr3t-token") || !strings.Contains(string(b), `"status":"failed"`) {
		t.Fatalf("payload = %s", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(work), "outside.txt")); err == nil {
		t.Fatal("a path outside the work directory was written")
	}
}
