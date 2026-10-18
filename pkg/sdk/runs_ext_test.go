package sdk_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/pkg/sdk"
)

func newStub(t *testing.T, h http.HandlerFunc) *sdk.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return sdk.NewClient(ts.URL, "key", sdk.WithMaxRetries(0))
}

func writeJSONBody(w http.ResponseWriter, status int, v string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(v))
}

func TestRunDecodesNewFields(t *testing.T) {
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSONBody(w, 200, `{"run_id":"run_1","state":"BLOCKED","blocked_reason":"human_request",
		"status_message":{"message":"waiting","failure_code":"x","retryable":true,"detail":"d"},
		"error":"old text","root_run_id":"run_0","parent_run_id":"run_p","depth":2,"active_child_runs":1,
		"session":3,"url":"https://x/runs/run_1","sandbox_running":true,
		"usage":{"input_tokens":5,"coverage":"partial"}}`)
	})
	r, err := c.Runs.Get(context.Background(), "run_1")
	if err != nil {
		t.Fatal(err)
	}
	sm := r.StatusMessage
	if sm == nil || sm.Message != "waiting" || sm.FailureCode != "x" || !sm.Retryable || sm.Detail != "d" {
		t.Fatalf("status_message = %+v", sm)
	}
	if r.Error != "old text" || r.BlockedReason != "human_request" || r.RootRunID != "run_0" || r.ParentRunID != "run_p" ||
		r.Depth != 2 || r.ActiveChildRuns != 1 || r.Session != 3 || r.URL == "" || !r.SandboxRunning || r.Usage.Coverage != "partial" {
		t.Fatalf("run = %+v", r)
	}
	if !r.WaitsForInput() || r.Finished() || r.Done() {
		t.Fatal("BLOCKED run must wait for input and not be finished")
	}
}

func TestStateHelpers(t *testing.T) {
	for _, s := range []string{sdk.StateSucceeded, sdk.StateFailed, sdk.StateError, sdk.StateCancelled} {
		if !sdk.Finished(s) || !sdk.Terminal(s) {
			t.Fatalf("%s must be finished", s)
		}
	}
	for _, s := range []string{sdk.StateQueued, sdk.StatePending, sdk.StateClaimed, sdk.StateRunning, sdk.StateBlocked} {
		if sdk.Finished(s) {
			t.Fatalf("%s must not be finished", s)
		}
	}
	if !sdk.WaitsForInput(sdk.StateBlocked) || sdk.WaitsForInput(sdk.StateRunning) {
		t.Fatal("WaitsForInput")
	}
}

func TestWaitLogic(t *testing.T) {
	run := func(states ...string) *sdk.Client {
		var n atomic.Int32
		return newStub(t, func(w http.ResponseWriter, r *http.Request) {
			i := int(n.Add(1)) - 1
			if i >= len(states) {
				i = len(states) - 1
			}
			writeJSONBody(w, 200, `{"run_id":"run_1","state":"`+states[i]+`"}`)
		})
	}
	ctx := context.Background()
	r, err := run("QUEUED", "RUNNING", "ERROR").Runs.Wait(ctx, "run_1", time.Millisecond, false)
	if err != nil || r.State != sdk.StateError {
		t.Fatal(r, err)
	}
	r, err = run("RUNNING", "BLOCKED", "SUCCEEDED").Runs.Wait(ctx, "run_1", time.Millisecond, true)
	if err != nil || r.State != sdk.StateBlocked {
		t.Fatalf("untilIdle must return at BLOCKED: %+v %v", r, err)
	}
	r, err = run("RUNNING", "BLOCKED", "SUCCEEDED").Runs.Wait(ctx, "run_1", time.Millisecond, false)
	if err != nil || r.State != sdk.StateSucceeded {
		t.Fatalf("a wait without untilIdle must pass BLOCKED: %+v %v", r, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if _, err := run("RUNNING").Runs.Wait(cctx, "run_1", time.Millisecond, true); err == nil {
		t.Fatal("wait must stop with the context")
	}
}

func TestWorkItemSendMessageAndRunInputRecord(t *testing.T) {
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/work-items/wi_1/messages":
			writeJSONBody(w, 202, `{"work_item_id":"wi_1","run_id":"run_1","message_id":"msg_7","outcome":"new_session"}`)
		case "GET /api/v1/runs/run_1/inputs":
			writeJSONBody(w, 200, `{"items":[{"message_id":"msg_7","seq":2,"kind":"lifecycle","state":"pending","content":"x","data":{"event":"succeeded"},"untrusted":true,"dedupe_key":"lifecycle:a:succeeded:1","accepted":false,"created_at":"2026-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			writeJSONBody(w, 404, `{"code":"resource_not_found","message":"x"}`)
		}
	})
	ctx := context.Background()
	res, err := c.WorkItems.SendMessage(ctx, "wi_1", "go on")
	if err != nil || res.MessageID != "msg_7" || res.Outcome != "new_session" || res.RunID != "run_1" {
		t.Fatal(res, err)
	}
	ins, err := c.Runs.Inputs(ctx, "run_1")
	if err != nil || len(ins) != 1 || !ins[0].Untrusted || ins[0].DedupeKey == "" || !strings.Contains(string(ins[0].Data), "succeeded") {
		t.Fatalf("inputs = %+v, %v", ins, err)
	}
}

func TestSendMessageInterruptCancel(t *testing.T) {
	var gotMsg map[string]any
	var gotCancel map[string]any
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/runs/run_1/messages":
			gotMsg = nil
			_ = json.Unmarshal(body, &gotMsg)
			writeJSONBody(w, 202, `{"run_id":"run_1","message_id":"msg_1","outcome":"queued"}`)
		case "POST /api/v1/runs/run_1/interrupt":
			writeJSONBody(w, 202, `{"status":"interrupting"}`)
		case "POST /api/v1/runs/run_2/interrupt":
			writeJSONBody(w, 409, `{"code":"conflict","message":"no active turn","reason":"no_active_turn"}`)
		case "POST /api/v1/runs/run_1/cancel":
			gotCancel = nil
			_ = json.Unmarshal(body, &gotCancel)
			writeJSONBody(w, 200, `{"run_id":"run_1"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			writeJSONBody(w, 404, `{"code":"resource_not_found","message":"x"}`)
		}
	})
	ctx := context.Background()
	res, err := c.Runs.SendMessage(ctx, "run_1", sdk.SendMessageParams{Message: "hi", Attachments: []string{"att_1"}})
	if err != nil || res.MessageID != "msg_1" || res.Outcome != "queued" {
		t.Fatal(res, err)
	}
	if gotMsg["message"] != "hi" || len(gotMsg["attachments"].([]any)) != 1 {
		t.Fatalf("body = %v", gotMsg)
	}
	if _, err := c.Runs.SendMessage(ctx, "run_1", sdk.SendMessageParams{Message: "plain"}); err != nil {
		t.Fatal(err)
	}
	if _, has := gotMsg["attachments"]; has {
		t.Fatalf("attachments must be optional: %v", gotMsg)
	}
	if err := c.Runs.Interrupt(ctx, "run_1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Runs.Interrupt(ctx, "run_2"); !sdk.IsConflict(err) {
		t.Fatalf("interrupt conflict: %v", err)
	}
	if err := c.Runs.Cancel(ctx, "run_1"); err != nil || gotCancel["cascade"] != true {
		t.Fatalf("Cancel must cascade: %v %v", gotCancel, err)
	}
	if err := c.Runs.CancelRun(ctx, "run_1", false); err != nil || gotCancel["cascade"] != false {
		t.Fatalf("CancelRun(false): %v %v", gotCancel, err)
	}
}

func TestRunReadEndpoints(t *testing.T) {
	var scope string
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/runs/run_1/timeline":
			writeJSONBody(w, 200, `{"items":[{"event_id":"e1","run_id":"run_1","session":1,"type":"session.started","at":"2026-01-01T00:00:00Z","data":{"a":1}}],"page_info":{"has_next_page":false}}`)
		case "/api/v1/runs/run_1/diagnostics":
			writeJSONBody(w, 200, `{"items":[{"code":"disk_low","message":"m","at":"2026-01-01T00:00:00Z","session":1}],"page_info":{"has_next_page":false}}`)
		case "/api/v1/runs/run_1/sessions":
			writeJSONBody(w, 200, `{"items":[{"session":1,"number":1,"state":"ended","assigned_at":"2026-01-01T00:00:00Z","end_reason":"done","usage":{"coverage":"known"},"snapshot":{"workspace":true,"harness_state":false}}],"page_info":{"has_next_page":false}}`)
		case "/api/v1/runs/run_1/inputs":
			writeJSONBody(w, 200, `{"items":[{"message_id":"msg_1","seq":1,"kind":"user","state":"consumed","content":"hi","accepted":true,"created_at":"2026-01-01T00:00:00Z"}],"page_info":{"has_next_page":false}}`)
		case "/api/v1/runs/run_1/conversation":
			scope = r.URL.Query().Get("scope")
			writeJSONBody(w, 200, `{"conversation_id":"conv_1","steps":[{"step_id":"s1","messages":[{"role":"assistant","blocks":[{"type":"text","text":"done"}]}]}]}`)
		default:
			t.Errorf("unexpected %s", r.URL)
			writeJSONBody(w, 404, `{"code":"resource_not_found","message":"x"}`)
		}
	})
	ctx := context.Background()
	tl, err := c.Runs.Timeline(ctx, "run_1")
	if err != nil || len(tl) != 1 || tl[0].Type != "session.started" || string(tl[0].Data) != `{"a":1}` {
		t.Fatal(tl, err)
	}
	dg, err := c.Runs.Diagnostics(ctx, "run_1")
	if err != nil || len(dg) != 1 || dg[0].Code != "disk_low" {
		t.Fatal(dg, err)
	}
	ss, err := c.Runs.Sessions(ctx, "run_1")
	if err != nil || len(ss) != 1 || ss[0].EndReason != "done" || !ss[0].Snapshot.Workspace || ss[0].Usage.Coverage != "known" {
		t.Fatal(ss, err)
	}
	in, err := c.Runs.Inputs(ctx, "run_1")
	if err != nil || len(in) != 1 || in[0].MessageID != "msg_1" || !in[0].Accepted {
		t.Fatal(in, err)
	}
	conv, err := c.Runs.Conversation(ctx, "run_1", nil)
	if err != nil || conv.ConversationID != "conv_1" || len(conv.Steps) != 1 || scope != "" {
		t.Fatal(conv, scope, err)
	}
	if _, err := c.Runs.Conversation(ctx, "run_1", &sdk.ConversationOptions{FinalMessageOnly: true}); err != nil || scope != "final_message" {
		t.Fatal(scope, err)
	}
}

func TestUploadAttachment(t *testing.T) {
	var name, ct, body string
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/attachments" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		name, ct = r.URL.Query().Get("name"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		writeJSONBody(w, 201, `{"id":"att_1","name":"a.txt","content_type":"text/plain","bytes":5,"created_at":"2026-01-01T00:00:00Z"}`)
	})
	att, err := c.Runs.UploadAttachment(context.Background(), "a.txt", "text/plain", strings.NewReader("hello"))
	if err != nil || att.ID != "att_1" || att.Bytes != 5 {
		t.Fatal(att, err)
	}
	if name != "a.txt" || ct != "text/plain" || body != "hello" {
		t.Fatalf("name %q, content type %q, body %q", name, ct, body)
	}
}
