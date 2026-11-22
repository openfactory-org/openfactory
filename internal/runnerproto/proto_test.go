package runnerproto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// strictDecode decodes raw into v and fails on unknown fields.
func strictDecode(t *testing.T, raw string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

const startResponseJSON = `{
  "session_token": "ofs_example",
  "session_token_expires_at": "2026-11-11T13:00:00Z",
  "run_id": "run_example",
  "session": 2,
  "conversation_id": "conv_example",
  "workspace": {
    "repositories": [
      {"forge": "GITHUB", "clone_url": "https://github.com/acme/api.git", "path": "api", "ref": "main"}
    ],
    "setup_commands": [{"origin": "machine", "command": "make deps"}],
    "setup_timeout_seconds": 1800,
    "skills": [{"name": "review", "files": {"SKILL.md": "skill text"}}],
    "snapshot": {"restore": true, "enabled": true}
  },
  "harness": {
    "type": "claude",
    "model": "model-name",
    "reasoning_level": "",
    "system_prompt": "prompt text",
    "resume_session_id": "harness-session",
    "mcp_servers": {"factory": {"url": "https://example.com/mcp/run/run_example", "auth": "session_token"}}
  },
  "env": {"OF_RUN_ID": "run_example", "OF_METADATA_TICKET": "ENG-1"},
  "idle_timeout_seconds": 600,
  "limits": {"max_session_seconds": 28800, "event_batch_bytes": 1048576}
}`

func TestStartResponseFromContractJSON(t *testing.T) {
	var r StartResponse
	strictDecode(t, startResponseJSON, &r)
	if r.Session != 2 {
		t.Errorf("Session = %d, want 2", r.Session)
	}
	if len(r.Workspace.Repositories) != 1 || r.Workspace.Repositories[0].CloneURL != "https://github.com/acme/api.git" {
		t.Errorf("Repositories = %+v", r.Workspace.Repositories)
	}
	if got := r.Harness.MCPServers["factory"].Auth; got != "session_token" {
		t.Errorf("MCP auth = %q", got)
	}
	if r.Limits.MaxSessionSeconds != 28800 {
		t.Errorf("MaxSessionSeconds = %d", r.Limits.MaxSessionSeconds)
	}
	want := time.Date(2026, 11, 11, 13, 0, 0, 0, time.UTC)
	if !r.SessionTokenExpiresAt.Equal(want) {
		t.Errorf("SessionTokenExpiresAt = %v", r.SessionTokenExpiresAt)
	}
	if r.Workspace.Skills[0].Files["SKILL.md"] != "skill text" || !r.Workspace.Snapshot.Restore {
		t.Errorf("workspace = %+v", r.Workspace)
	}
}

func TestStartRequestFromContractJSON(t *testing.T) {
	var r StartRequest
	strictDecode(t, `{"instance_id": "b64url-128-bit", "runner_version": "0.9.0", "sandbox_id": "job-abc", "platform": {"os": "linux", "arch": "amd64"}}`, &r)
	if r.SandboxID != "job-abc" || r.Platform.Arch != "amd64" {
		t.Errorf("got %+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestSecretsResponseFromContractJSON(t *testing.T) {
	var r SecretsResponse
	strictDecode(t, `{
  "env": {"NPM_TOKEN": "secret"},
  "files": [{"path": ".openfactory/secrets/gcp.json", "mode": "0600", "content_base64": "e30="}],
  "harness_auth": {"ANTHROPIC_API_KEY": "secret"},
  "mcp_headers": {"internal-docs": {"Authorization": "Bearer secret"}},
  "redact": ["one", "two"]
}`, &r)
	if r.Files[0].Mode != "0600" || r.Files[0].ContentBase64 != "e30=" {
		t.Errorf("Files = %+v", r.Files)
	}
	if r.MCPHeaders["internal-docs"]["Authorization"] != "Bearer secret" {
		t.Errorf("MCPHeaders = %+v", r.MCPHeaders)
	}
	if len(r.Redact) != 2 || r.HarnessAuth["ANTHROPIC_API_KEY"] != "secret" {
		t.Errorf("got %+v", r)
	}
}

func TestEventBatchFromContractJSON(t *testing.T) {
	var b EventBatch
	strictDecode(t, `{"events": [
  {"seq": 41, "type": "message", "turn_id": "turn_x", "at": "2026-11-11T12:00:00Z", "data": {"role": "assistant", "content": "hello"}},
  {"seq": 42, "type": "usage", "turn_id": "turn_x", "at": "2026-11-11T12:00:01Z", "data": {"input_tokens": 1200, "output_tokens": 300, "cumulative": true}}
]}`, &b)
	if len(b.Events) != 2 {
		t.Fatalf("events = %d", len(b.Events))
	}
	var m MessageData
	if err := b.Events[0].Decode(&m); err != nil || m.Role != "assistant" || m.Content != "hello" {
		t.Errorf("message = %+v, err %v", m, err)
	}
	var u UsageData
	if err := b.Events[1].Decode(&u); err != nil || u.InputTokens != 1200 || !u.Cumulative {
		t.Errorf("usage = %+v, err %v", u, err)
	}
	if err := b.Validate(40); err != nil {
		t.Errorf("Validate: %v", err)
	}
	var ack EventAck
	strictDecode(t, `{"ack": 42}`, &ack)
	if ack.Ack != 42 {
		t.Errorf("ack = %d", ack.Ack)
	}
}

func TestCommandsResponseFromContractJSON(t *testing.T) {
	var r CommandsResponse
	strictDecode(t, `{"commands": [
  {"seq": 3, "type": "turn.start", "data": {"turn_id": "turn_x", "input": [
    {"message_id": "msg_x", "kind": "user", "sender": {"type": "user", "name": "Dana"}, "content": "do it", "attachments": []}
  ]}}
]}`, &r)
	if len(r.Commands) != 1 || r.Commands[0].Type != CommandTurnStart || r.Commands[0].Seq != 3 {
		t.Fatalf("commands = %+v", r.Commands)
	}
	var d TurnStartData
	if err := r.Commands[0].Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.TurnID != "turn_x" || len(d.Input) != 1 {
		t.Fatalf("data = %+v", d)
	}
	in := d.Input[0]
	if in.MessageID != "msg_x" || in.Kind != KindUser || in.Sender.Name != "Dana" || in.Content != "do it" {
		t.Errorf("input = %+v", in)
	}
}

func TestEndRequestFromContractJSON(t *testing.T) {
	var r EndRequest
	strictDecode(t, `{"reason": "idle", "last_event_seq": 97, "failure_code": "", "message": "", "exit": {"code": 0}}`, &r)
	if r.Reason != EndIdle || r.LastEventSeq != 97 || r.Exit == nil || r.Exit.Code != 0 {
		t.Errorf("got %+v", r)
	}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestUploadFromContractJSON(t *testing.T) {
	var req UploadRequest
	strictDecode(t, `{"kind": "artifact", "name": "report.html", "bytes": 48213, "sha256": "`+strings.Repeat("a", 64)+`", "content_type": "text/html"}`, &req)
	if req.Kind != UploadArtifact || req.Bytes != 48213 || req.ContentType != "text/html" {
		t.Errorf("request = %+v", req)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	var resp UploadResponse
	strictDecode(t, `{"upload_id": "up_x", "method": "PUT", "url": "https://example.com/put", "headers": {}, "expires_at": "2026-11-11T13:00:00Z"}`, &resp)
	if resp.UploadID != "up_x" || resp.Method != "PUT" || resp.ExpiresAt.IsZero() {
		t.Errorf("response = %+v", resp)
	}
}

func TestDownloadsFromContractJSON(t *testing.T) {
	var r DownloadsResponse
	strictDecode(t, `{"files": [{"part": "workspace", "name": "a.txt", "url": "https://example.com/get", "bytes": 3, "expires_at": "2026-11-11T13:00:00Z"}]}`, &r)
	if len(r.Files) != 1 || r.Files[0].Part != "workspace" {
		t.Errorf("got %+v", r)
	}
}

func TestGitCredentialFromContractJSON(t *testing.T) {
	var req GitCredentialRequest
	strictDecode(t, `{"protocol": "https", "host": "github.com", "path": "acme/api.git"}`, &req)
	if req.Host != "github.com" || req.Path != "acme/api.git" {
		t.Errorf("request = %+v", req)
	}
	var resp GitCredentialResponse
	strictDecode(t, `{"username": "x-access-token", "password": "secret", "expires_at": "2026-11-11T13:00:00Z"}`, &resp)
	if resp.Username != "x-access-token" || resp.Password != "secret" {
		t.Errorf("response = %+v", resp)
	}
}

func TestCloudTokenFromContractJSON(t *testing.T) {
	var req CloudTokenRequest
	strictDecode(t, `{"audience": "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/of", "ttl_seconds": 3600}`, &req)
	if req.Audience != "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/of" || req.TTLSeconds != 3600 {
		t.Errorf("request = %+v", req)
	}
	var resp CloudTokenResponse
	strictDecode(t, `{"token": "tok", "expires_at": "2026-11-11T13:00:00Z"}`, &resp)
	if resp.Token != "tok" {
		t.Errorf("response = %+v", resp)
	}
	var tr TokenResponse
	strictDecode(t, `{"session_token": "ofs_x", "session_token_expires_at": "2026-11-11T13:00:00Z"}`, &tr)
	if tr.SessionToken != "ofs_x" {
		t.Errorf("token response = %+v", tr)
	}
}

func TestEventBatchValidate(t *testing.T) {
	mk := func(typ EventType, seqs ...int64) EventBatch {
		var b EventBatch
		for _, s := range seqs {
			b.Events = append(b.Events, Event{Seq: s, Type: typ})
		}
		return b
	}
	tests := []struct {
		name     string
		batch    EventBatch
		afterSeq int64
		wantErr  bool
	}{
		{"valid after zero", mk(EventMessage, 1, 2, 3), 0, false},
		{"valid repeat", mk(EventMessage, 3, 4, 5, 6), 5, false},
		{"empty", EventBatch{}, 0, true},
		{"gap at start", mk(EventMessage, 7, 8), 5, true},
		{"gap inside", mk(EventMessage, 6, 7, 9), 5, true},
		{"invalid type", mk("nope", 1), 0, true},
		{"seq zero", mk(EventMessage, 0), 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.batch.Validate(tc.afterSeq)
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewEventDecodeRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	in := TurnEndedData{Outcome: OutcomeError, Result: "r", FailureCode: "f", Message: "m"}
	e, err := NewEvent(5, EventTurnEnded, "turn_x", at, in)
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 5 || e.Type != EventTurnEnded || e.TurnID != "turn_x" || !e.At.Equal(at) {
		t.Errorf("event = %+v", e)
	}
	var out TurnEndedData
	if err := e.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
	empty, err := NewEvent(1, EventTurnStarted, "", at, nil)
	if err != nil || len(empty.Data) != 0 {
		t.Errorf("nil data: %+v, %v", empty, err)
	}
	keep := TurnEndedData{Message: "keep"}
	if err := empty.Decode(&keep); err != nil || keep.Message != "keep" {
		t.Errorf("empty decode changed value: %+v, %v", keep, err)
	}
}

func TestNewCommandDecodeRoundTrip(t *testing.T) {
	in := TurnStartData{TurnID: "turn_x", Input: []InputItem{{
		MessageID:   "msg_x",
		Kind:        KindAgent,
		Sender:      Sender{Type: "run", ID: "run_y"},
		Content:     "hi",
		Untrusted:   true,
		Attachments: []Attachment{{ID: "att_x", Name: "a.txt", ContentType: "text/plain", Bytes: 2, Path: "a.txt"}},
	}}}
	c, err := NewCommand(3, CommandTurnStart, in)
	if err != nil {
		t.Fatal(err)
	}
	if c.Seq != 3 || c.Type != CommandTurnStart {
		t.Errorf("command = %+v", c)
	}
	var out TurnStartData
	if err := c.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.TurnID != in.TurnID || len(out.Input) != 1 || out.Input[0].Attachments[0] != in.Input[0].Attachments[0] ||
		out.Input[0].Sender != in.Input[0].Sender || !out.Input[0].Untrusted {
		t.Errorf("round trip = %+v", out)
	}
	stop, err := NewCommand(4, CommandSessionCancel, nil)
	if err != nil || len(stop.Data) != 0 {
		t.Errorf("nil data: %+v, %v", stop, err)
	}
}

func TestValidEnums(t *testing.T) {
	tests := []struct {
		name  string
		valid []bool
		nope  bool
	}{
		{"EventType", []bool{
			EventSetupStep.Valid(), EventSnapshotRestored.Valid(), EventSessionReady.Valid(), EventTurnStarted.Valid(),
			EventInputAccepted.Valid(), EventMessage.Valid(), EventToolCall.Valid(), EventToolResult.Valid(),
			EventHarnessSession.Valid(), EventUsage.Valid(), EventArtifact.Valid(), EventTranscriptSaved.Valid(),
			EventDiagnostic.Valid(), EventTurnEnded.Valid(), EventIdle.Valid(), EventSnapshotSaved.Valid(),
		}, EventType("nope").Valid()},
		{"CommandType", []bool{
			CommandTurnStart.Valid(), CommandMessageDeliver.Valid(), CommandTurnInterrupt.Valid(),
			CommandSessionStop.Valid(), CommandSessionCancel.Valid(),
		}, CommandType("nope").Valid()},
		{"EndReason", []bool{
			EndIdle.Valid(), EndStopped.Valid(), EndCancelled.Valid(), EndSetupFailed.Valid(), EndDeadline.Valid(), EndFatal.Valid(),
		}, EndReason("nope").Valid()},
		{"TurnOutcome", []bool{
			OutcomeDone.Valid(), OutcomeError.Valid(), OutcomeInterrupted.Valid(), OutcomeNeedsInput.Valid(),
		}, TurnOutcome("nope").Valid()},
		{"Capability", []bool{
			CapResume.Valid(), CapMidTurnInput.Valid(), CapInterrupt.Valid(), CapNeedsInputSignal.Valid(),
		}, Capability("nope").Valid()},
		{"MessageKind", []bool{
			KindUser.Valid(), KindAgent.Valid(), KindLifecycle.Valid(), KindEvent.Valid(), KindSystem.Valid(),
		}, MessageKind("nope").Valid()},
		{"UploadKind", []bool{
			UploadArtifact.Valid(), UploadSnapshot.Valid(), UploadTranscript.Valid(),
		}, UploadKind("nope").Valid()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for i, v := range tc.valid {
				if !v {
					t.Errorf("constant %d is not valid", i)
				}
			}
			if tc.nope {
				t.Error(`"nope" is valid`)
			}
		})
	}
}

func TestHasCapability(t *testing.T) {
	list := []Capability{CapResume, CapInterrupt}
	if !HasCapability(list, CapInterrupt) || HasCapability(list, CapMidTurnInput) || HasCapability(nil, CapResume) {
		t.Error("HasCapability gave a wrong answer")
	}
}

func TestEndRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     EndRequest
		wantErr bool
	}{
		{"idle", EndRequest{Reason: EndIdle, LastEventSeq: 5}, false},
		{"zero seq", EndRequest{Reason: EndStopped}, false},
		{"invalid reason", EndRequest{Reason: "nope"}, true},
		{"empty reason", EndRequest{}, true},
		{"negative seq", EndRequest{Reason: EndIdle, LastEventSeq: -1}, true},
		{"setup_failed without code", EndRequest{Reason: EndSetupFailed}, true},
		{"setup_failed with code", EndRequest{Reason: EndSetupFailed, FailureCode: "setup_command_failed"}, false},
		{"fatal without code", EndRequest{Reason: EndFatal}, true},
		{"fatal with code", EndRequest{Reason: EndFatal, FailureCode: "runner_error"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.req.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestStartRequestValidate(t *testing.T) {
	ok := StartRequest{InstanceID: "i", RunnerVersion: "0.9.0", SandboxID: "s", Platform: Platform{OS: "linux", Arch: "amd64"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid request: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*StartRequest)
	}{
		{"no instance_id", func(r *StartRequest) { r.InstanceID = "" }},
		{"no runner_version", func(r *StartRequest) { r.RunnerVersion = "" }},
		{"no os", func(r *StartRequest) { r.Platform.OS = "" }},
		{"no arch", func(r *StartRequest) { r.Platform.Arch = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := ok
			tc.mutate(&r)
			if err := r.Validate(); err == nil {
				t.Error("want error")
			}
		})
	}
}

func TestUploadRequestValidate(t *testing.T) {
	hash := strings.Repeat("0123456789abcdef", 4)
	base := func() UploadRequest {
		return UploadRequest{Kind: UploadArtifact, Name: "report.html", Bytes: 10, SHA256: hash}
	}
	tests := []struct {
		name    string
		mutate  func(*UploadRequest)
		wantErr bool
	}{
		{"valid artifact", func(r *UploadRequest) {}, false},
		{"zero bytes", func(r *UploadRequest) { r.Bytes = 0 }, false},
		{"valid transcript", func(r *UploadRequest) { r.Kind = UploadTranscript }, false},
		{"valid snapshot workspace", func(r *UploadRequest) { r.Kind = UploadSnapshot; r.Part = "workspace" }, false},
		{"valid snapshot harness_state", func(r *UploadRequest) { r.Kind = UploadSnapshot; r.Part = "harness_state" }, false},
		{"nested name", func(r *UploadRequest) { r.Name = "a/b/c.txt" }, false},
		{"dots in file name", func(r *UploadRequest) { r.Name = "a..b" }, false},
		{"invalid kind", func(r *UploadRequest) { r.Kind = "nope" }, true},
		{"empty name", func(r *UploadRequest) { r.Name = "" }, true},
		{"absolute name", func(r *UploadRequest) { r.Name = "/etc/passwd" }, true},
		{"dotdot element", func(r *UploadRequest) { r.Name = "a/../b" }, true},
		{"dotdot only", func(r *UploadRequest) { r.Name = ".." }, true},
		{"negative bytes", func(r *UploadRequest) { r.Bytes = -1 }, true},
		{"short hash", func(r *UploadRequest) { r.SHA256 = "abc" }, true},
		{"upper-case hash", func(r *UploadRequest) { r.SHA256 = strings.ToUpper(hash) }, true},
		{"non-hex hash", func(r *UploadRequest) { r.SHA256 = strings.Repeat("g", 64) }, true},
		{"snapshot without part", func(r *UploadRequest) { r.Kind = UploadSnapshot }, true},
		{"snapshot bad part", func(r *UploadRequest) { r.Kind = UploadSnapshot; r.Part = "other" }, true},
		{"artifact with part", func(r *UploadRequest) { r.Part = "workspace" }, true},
		{"transcript with part", func(r *UploadRequest) { r.Kind = UploadTranscript; r.Part = "workspace" }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mutate(&r)
			if err := r.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestClampWait(t *testing.T) {
	tests := []struct{ in, want int }{{-1, 0}, {0, 0}, {10, 10}, {25, 25}, {26, 25}}
	for _, tc := range tests {
		if got := ClampWait(tc.in); got != tc.want {
			t.Errorf("ClampWait(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestEventTypeValues(t *testing.T) {
	tests := []struct {
		want string
		got  EventType
	}{
		{"setup.step", EventSetupStep},
		{"snapshot.restored", EventSnapshotRestored},
		{"session.ready", EventSessionReady},
		{"turn.started", EventTurnStarted},
		{"input.accepted", EventInputAccepted},
		{"message", EventMessage},
		{"tool_call", EventToolCall},
		{"tool_result", EventToolResult},
		{"harness.session", EventHarnessSession},
		{"usage", EventUsage},
		{"artifact", EventArtifact},
		{"transcript.saved", EventTranscriptSaved},
		{"diagnostic", EventDiagnostic},
		{"turn.ended", EventTurnEnded},
		{"idle", EventIdle},
		{"snapshot.saved", EventSnapshotSaved},
	}
	if len(tests) != 16 {
		t.Fatalf("table has %d rows, want 16", len(tests))
	}
	for _, tc := range tests {
		if string(tc.got) != tc.want {
			t.Errorf("constant = %q, want %q", tc.got, tc.want)
		}
	}
}
