package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// TestEventContextThread checks the thread context for chat: the builder returns the earlier
// messages of the thread and the files it can read, and lists the others.
func TestEventContextThread(t *testing.T) {
	var srv *httptest.Server
	var tokens []string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/conversations.replies"):
			if r.URL.Query().Get("ts") != "1.0" || r.URL.Query().Get("channel") != "C1" {
				http.Error(w, "bad", 400)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"messages":[
				{"ts":"1.0","user":"U1","text":"The staging deploy broke at 10:02.","files":[
					{"id":"F1","name":"log.txt","size":5,"url_private_download":"%[1]s/files/F1"},
					{"id":"F2","name":"big.bin","size":99999999,"url_private_download":"%[1]s/files/F2"},
					{"id":"F3","name":"x.txt","size":1,"url_private_download":"https://evil.example/F3"}]},
				{"ts":"1.1","bot_id":"B1","text":"On it."}]}`, srv.URL)
		case r.URL.Path == "/files/F1":
			_, _ = w.Write([]byte("oops!"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_s", TeamID: "team_1", Provider: "slack", SecretRef: "SLACK_BOT", FactoryID: "fac_1", Config: map[string]string{"team_id": "T1"}}}
	r := NewResponder(ResponderOptions{Store: store, Secrets: forgetest.NewSecrets(map[string]string{"SLACK_BOT": "xoxb-test"}), APIBaseURL: srv.URL})
	ev := &core.Event{TeamID: "team_1", FactoryID: "fac_1", Provider: "slack", Source: core.SourceRef{Provider: "slack", Channel: "C1", ThreadID: "1.0"}}
	files, omitted, err := r.EventContext(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "thread.json" || files[1].Path != "files/F1-log.txt" || string(files[1].Data) != "oops!" {
		t.Fatalf("files = %+v", files)
	}
	var thread []ThreadEntry
	if err := json.Unmarshal(files[0].Data, &thread); err != nil || len(thread) != 2 || thread[0].Text != "The staging deploy broke at 10:02." || !thread[1].Bot {
		t.Fatalf("thread = %+v, %v", thread, err)
	}
	if len(omitted) != 2 || omitted[0].Name != "big.bin" || omitted[1].Name != "x.txt" {
		t.Fatalf("omitted = %+v", omitted)
	}
	for _, tok := range tokens {
		if tok != "Bearer xoxb-test" {
			t.Fatalf("authorization = %q", tok)
		}
	}
	// No thread: no context and no call.
	n := len(tokens)
	if f, _, err := r.EventContext(context.Background(), &core.Event{TeamID: "team_1", Provider: "slack"}); err != nil || f != nil || len(tokens) != n {
		t.Fatalf("no thread: files=%v err=%v", f, err)
	}
}
