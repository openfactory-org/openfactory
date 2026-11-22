// Package runnertest is an in-memory fake of the control-plane side of the
// runner protocol (/runner/v1). Runner tests start the real runner against
// it. Every request body that the runner sends is validated against
// schemas/runner/v1; a body that does not match fails the test.
package runnertest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/testkit/schematest"
)

// Session states.
const (
	SessionAssigned = "assigned"
	SessionActive   = "active"
	SessionEnded    = "ended"
)

// Event is one stored runner event.
type Event struct {
	Seq    int64
	Type   string
	TurnID string
	At     time.Time
	Data   json.RawMessage
}

// Session is the state of one session, as the fake sees it.
type Session struct {
	N             int
	State         string
	InstanceID    string
	RunnerVersion string
	SandboxID     string
	ReadyAt       *time.Time
	EndReason     string
	FailureCode   string
	End           *runnerproto.EndRequest
	EventAck      int64
	Capabilities  []string
	ActiveTurnID  string
}

// Options configure a Server.
type Options struct {
	// RunID and TeamID of the run. Defaults: "run_test" and "team_1".
	RunID, TeamID string
	// RunnerVersion, when set, is the only runner version that the session
	// start accepts (409 runner_version_mismatch otherwise).
	RunnerVersion string
	// PollInterval is how often a commands long poll checks the session.
	// Default 50 ms.
	PollInterval time.Duration
	// SchemaDir is the directory of the runner schemas. Default: found from
	// the source file of this package.
	SchemaDir string
}

type session struct {
	Session
	bootstrap string
	start     runnerproto.StartResponse
	events    []Event
	commands  []runnerproto.Command
	wake      chan struct{}
}

// Server is the fake control plane.
type Server struct {
	t    testing.TB
	opts Options
	srv  *httptest.Server
	mux  *http.ServeMux
	v    *schematest.Validator

	mu        sync.Mutex
	sessions  map[int]*session
	tokens    map[string]int
	secrets   runnerproto.SecretsResponse
	objects   map[string][]byte
	putStatus int
	puts      int
	messages  []message
	cancelled bool

	// GitCredential answers git credential requests. Nil answers 501.
	GitCredential func(req runnerproto.GitCredentialRequest) (runnerproto.GitCredentialResponse, int)
	// CloudToken answers cloud token requests. Nil answers 501.
	CloudToken func(req runnerproto.CloudTokenRequest) (runnerproto.CloudTokenResponse, int)
	// OnEnd is called for each session end request that the fake accepts.
	OnEnd func(session int, req runnerproto.EndRequest)
}

type message struct{ role, content string }

// New starts a fake control plane. It stops at the end of the test.
func New(t testing.TB, opts Options) *Server {
	t.Helper()
	if opts.RunID == "" {
		opts.RunID = "run_test"
	}
	if opts.TeamID == "" {
		opts.TeamID = "team_1"
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 50 * time.Millisecond
	}
	if opts.SchemaDir == "" {
		_, file, _, _ := runtime.Caller(0)
		opts.SchemaDir = filepath.Join(filepath.Dir(file), "..", "..", "..", "schemas", "runner", "v1")
	}
	s := &Server{
		t: t, opts: opts, v: schematest.New(t, opts.SchemaDir),
		sessions: map[int]*session{}, tokens: map[string]int{}, objects: map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+runnerproto.PathSessionStart, s.handleStart)
	mux.HandleFunc("GET "+runnerproto.PathSessionSecrets, s.auth(s.handleSecrets))
	mux.HandleFunc("POST "+runnerproto.PathSessionEvents, s.auth(s.handleEvents))
	mux.HandleFunc("GET "+runnerproto.PathSessionCommands, s.auth(s.handleCommands))
	mux.HandleFunc("POST "+runnerproto.PathSessionEnd, s.auth(s.handleEnd))
	mux.HandleFunc("POST "+runnerproto.PathSessionUploads, s.auth(s.handleUpload))
	mux.HandleFunc("GET "+runnerproto.PathSessionDownloads, s.auth(s.handleDownloads))
	mux.HandleFunc("POST "+runnerproto.PathGitCredential, s.auth(s.handleGitCredential))
	mux.HandleFunc("POST "+runnerproto.PathCloudToken, s.auth(s.handleCloudToken))
	mux.HandleFunc("POST "+runnerproto.PathSessionToken, s.auth(s.handleToken))
	mux.HandleFunc("GET "+runnerproto.PathConversation, s.auth(s.handleConversation))
	mux.HandleFunc("/obj/", s.handleObject)
	s.mux = mux
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the base URL of the fake (the control URL of the runner).
func (s *Server) URL() string { return s.srv.URL }

// Handler is the HTTP handler of the fake, for a test that puts a proxy in front of it.
func (s *Server) Handler() http.Handler { return s.mux }

// RunID is the ID of the run.
func (s *Server) RunID() string { return s.opts.RunID }

func randomToken(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// AddSession adds session n, waiting for a runner, with the start response
// resp. It returns the bootstrap token of the session.
func (s *Server) AddSession(n int, resp runnerproto.StartResponse) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := randomToken("ofb_")
	s.sessions[n] = &session{Session: Session{N: n, State: SessionAssigned}, bootstrap: tok, start: resp, wake: make(chan struct{})}
	return tok
}

// SetStartResponse replaces the start response of session n.
func (s *Server) SetStartResponse(n int, resp runnerproto.StartResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if se := s.sessions[n]; se != nil {
		se.start = resp
	}
}

// SetSecrets sets the answer of the secrets request.
func (s *Server) SetSecrets(resp runnerproto.SecretsResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets = resp
}

// SetRunCancelled makes the session start answer 410 run_cancelled.
func (s *Server) SetRunCancelled(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = v
}

// SetPutStatus makes each object PUT answer with status (0: store it).
func (s *Server) SetPutStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putStatus = status
}

// AddMessage adds a message to the conversation of the run.
func (s *Server) AddMessage(role, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, message{role, content})
}

// Session returns a copy of the state of session n.
func (s *Server) Session(n int) Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.sessions[n]
	if se == nil {
		s.t.Fatalf("runnertest: no session %d", n)
	}
	return se.Session
}

// Events returns the stored events of session n.
func (s *Server) Events(n int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.sessions[n]
	if se == nil {
		return nil
	}
	return append([]Event(nil), se.events...)
}

// Enqueue appends a command for session n and wakes its long poll.
func (s *Server) Enqueue(n int, typ runnerproto.CommandType, data any) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.sessions[n]
	if se == nil {
		s.t.Fatalf("runnertest: no session %d", n)
	}
	c, err := runnerproto.NewCommand(int64(len(se.commands)+1), typ, data)
	if err != nil {
		s.t.Fatal(err)
	}
	se.commands = append(se.commands, c)
	s.wakeLocked(se)
}

func (s *Server) wakeLocked(se *session) {
	close(se.wake)
	se.wake = make(chan struct{})
}

// Object returns a stored object.
func (s *Server) Object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[key]
	return b, ok
}

// Objects returns a copy of the stored objects by key.
func (s *Server) Objects() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]byte, len(s.objects))
	for k, v := range s.objects {
		out[k] = v
	}
	return out
}

// ObjectKey returns the object key of an upload ID.
func (s *Server) ObjectKey(uploadID string) (string, bool) {
	rest, ok := strings.CutPrefix(uploadID, "up_")
	if !ok {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func uploadID(key string) string { return "up_" + base64.RawURLEncoding.EncodeToString([]byte(key)) }

// ObjectURL is the URL of an object of the fake object store.
func (s *Server) ObjectURL(key string) string { return s.srv.URL + "/obj/" + key }

// ---------- HTTP ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, runnerproto.ErrorBody{Code: code, Message: msg})
}

// decode reads a JSON body and validates it against a schema definition.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, ref string, v any) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	if err := s.v.ValidateJSON(ref, data); err != nil {
		s.t.Errorf("runnertest: %s %s: the body does not match %s: %v\n%s", r.Method, r.URL.Path, ref, err, data)
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	if err := json.Unmarshal(data, v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return false
	}
	return true
}

func bearer(r *http.Request) string {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, n int)

// auth checks the session token: the session exists, is not ended and is
// the newest session.
func (s *Server) auth(h sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n, ok := s.tokens[bearer(r)]
		se := s.sessions[n]
		latest := 0
		for k := range s.sessions {
			latest = max(latest, k)
		}
		s.mu.Unlock()
		switch {
		case !ok || se == nil:
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid session token")
			return
		case se.State == SessionEnded:
			writeError(w, http.StatusUnauthorized, "unauthorized", "session ended")
			return
		case latest > n:
			writeError(w, http.StatusConflict, runnerproto.CodeSessionSuperseded, "a newer session exists")
			return
		}
		h(w, r, n)
	}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req runnerproto.StartRequest
	if !s.decode(w, r, "session.json#/$defs/StartRequest", &req) {
		return
	}
	tok := bearer(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	var se *session
	for _, x := range s.sessions {
		if x.bootstrap != "" && x.bootstrap == tok {
			se = x
		}
	}
	switch {
	case se == nil:
		writeError(w, http.StatusUnauthorized, "unauthorized", "unknown bootstrap token")
		return
	case s.cancelled:
		writeError(w, http.StatusGone, runnerproto.CodeRunCancelled, "the run was cancelled")
		return
	case se.State == SessionEnded:
		writeError(w, http.StatusUnauthorized, "unauthorized", "session ended")
		return
	case s.opts.RunnerVersion != "" && req.RunnerVersion != s.opts.RunnerVersion:
		writeError(w, http.StatusConflict, runnerproto.CodeRunnerVersionMismatch, "the control plane serves runner "+s.opts.RunnerVersion)
		return
	}
	if se.State == SessionAssigned {
		se.State = SessionActive
		se.InstanceID = req.InstanceID
		se.RunnerVersion = req.RunnerVersion
		se.SandboxID = req.SandboxID
	}
	if se.InstanceID != req.InstanceID {
		writeError(w, http.StatusUnauthorized, "unauthorized", "bootstrap token is bound to another instance")
		return
	}
	resp := se.start
	token := randomToken("ofs_")
	s.tokens[token] = se.N
	resp.RunID = s.opts.RunID
	resp.Session = se.N
	resp.SessionToken = token
	resp.SessionTokenExpiresAt = time.Now().Add(time.Hour).UTC()
	if resp.ConversationID == "" {
		resp.ConversationID = s.opts.RunID
	}
	if resp.Limits.EventBatchBytes == 0 {
		resp.Limits.EventBatchBytes = runnerproto.DefaultEventBatchBytes
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSecrets(w http.ResponseWriter, _ *http.Request, _ int) {
	s.mu.Lock()
	resp := s.secrets
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// eventDataRef maps an event type to the schema of its data.
var eventDataRef = map[runnerproto.EventType]string{
	runnerproto.EventSetupStep:        "SetupStepData",
	runnerproto.EventSnapshotRestored: "SnapshotRestoredData",
	runnerproto.EventSessionReady:     "SessionReadyData",
	runnerproto.EventInputAccepted:    "InputAcceptedData",
	runnerproto.EventMessage:          "MessageData",
	runnerproto.EventToolCall:         "ToolCallData",
	runnerproto.EventToolResult:       "ToolResultData",
	runnerproto.EventHarnessSession:   "HarnessSessionData",
	runnerproto.EventUsage:            "UsageData",
	runnerproto.EventArtifact:         "ArtifactData",
	runnerproto.EventTranscriptSaved:  "TranscriptSavedData",
	runnerproto.EventDiagnostic:       "DiagnosticData",
	runnerproto.EventTurnEnded:        "TurnEndedData",
	runnerproto.EventIdle:             "IdleData",
	runnerproto.EventSnapshotSaved:    "SnapshotSavedData",
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, n int) {
	var batch runnerproto.EventBatch
	if !s.decode(w, r, "events.json#/$defs/EventBatch", &batch) {
		return
	}
	for _, e := range batch.Events {
		if def, ok := eventDataRef[e.Type]; ok && len(e.Data) > 0 {
			if err := s.v.ValidateJSON("events.json#/$defs/"+def, e.Data); err != nil {
				s.t.Errorf("runnertest: event %d (%s): the data does not match %s: %v\n%s", e.Seq, e.Type, def, err, e.Data)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.sessions[n]
	for _, e := range batch.Events {
		if e.Seq <= se.EventAck {
			continue // a resend
		}
		if e.Seq != se.EventAck+1 {
			ack := se.EventAck
			writeJSON(w, http.StatusConflict, runnerproto.ErrorBody{Code: "event_gap", Message: "event gap", Ack: &ack})
			return
		}
		ev := Event{Seq: e.Seq, Type: string(e.Type), TurnID: e.TurnID, At: e.At, Data: e.Data}
		se.events = append(se.events, ev)
		se.EventAck = e.Seq
		s.applyLocked(se, ev)
	}
	writeJSON(w, http.StatusOK, runnerproto.EventAck{Ack: se.EventAck})
}

func (s *Server) applyLocked(se *session, ev Event) {
	switch runnerproto.EventType(ev.Type) {
	case runnerproto.EventSessionReady:
		var d runnerproto.SessionReadyData
		_ = json.Unmarshal(ev.Data, &d)
		se.Capabilities = se.Capabilities[:0]
		for _, c := range d.Capabilities {
			se.Capabilities = append(se.Capabilities, string(c))
		}
		if se.ReadyAt == nil {
			now := time.Now()
			se.ReadyAt = &now
		}
	case runnerproto.EventTurnStarted:
		se.ActiveTurnID = ev.TurnID
	case runnerproto.EventTurnEnded:
		if se.ActiveTurnID == ev.TurnID {
			se.ActiveTurnID = ""
		}
	}
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request, n int) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	wait, _ := strconv.Atoi(q.Get("wait"))
	deadline := time.After(time.Duration(runnerproto.ClampWait(wait)) * time.Second)
	for {
		s.mu.Lock()
		se := s.sessions[n]
		var out []runnerproto.Command
		for _, c := range se.commands {
			if c.Seq > after {
				out = append(out, c)
			}
		}
		ended := se.State == SessionEnded
		wake := se.wake
		s.mu.Unlock()
		if ended {
			writeError(w, http.StatusUnauthorized, "unauthorized", "session ended")
			return
		}
		if len(out) > 0 || wait <= 0 {
			if out == nil {
				out = []runnerproto.Command{}
			}
			writeJSON(w, http.StatusOK, runnerproto.CommandsResponse{Commands: out})
			return
		}
		select {
		case <-wake:
		case <-time.After(s.opts.PollInterval):
		case <-deadline:
			writeJSON(w, http.StatusOK, runnerproto.CommandsResponse{Commands: []runnerproto.Command{}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request, n int) {
	var req runnerproto.EndRequest
	if !s.decode(w, r, "session.json#/$defs/EndRequest", &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	s.mu.Lock()
	se := s.sessions[n]
	if req.LastEventSeq > se.EventAck {
		ack := se.EventAck
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, runnerproto.ErrorBody{Code: "event_gap", Message: "event gap", Ack: &ack})
		return
	}
	se.State = SessionEnded
	se.EndReason = string(req.Reason)
	se.FailureCode = req.FailureCode
	se.ActiveTurnID = ""
	cp := req
	se.End = &cp
	s.wakeLocked(se)
	hook := s.OnEnd
	s.mu.Unlock()
	if hook != nil {
		hook(n, req)
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// objectKey returns the key of an upload, with the layout of the control plane.
func (s *Server) objectKey(n int, req runnerproto.UploadRequest) string {
	ns := strconv.Itoa(n)
	if req.Kind == runnerproto.UploadSnapshot {
		return "snapshots/" + s.opts.TeamID + "/" + s.opts.RunID + "/" + ns + "/" + req.Part + "/" + req.Name
	}
	return "runs/" + s.opts.RunID + "/sessions/" + ns + "/" + string(req.Kind) + "/" + req.Name
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, n int) {
	var req runnerproto.UploadRequest
	if !s.decode(w, r, "transfer.json#/$defs/UploadRequest", &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	key := s.objectKey(n, req)
	headers := map[string]string{}
	if req.ContentType != "" {
		headers["Content-Type"] = req.ContentType
	}
	writeJSON(w, http.StatusOK, runnerproto.UploadResponse{
		UploadID: uploadID(key), Method: http.MethodPut, URL: s.ObjectURL(key), Headers: headers,
		ExpiresAt: time.Now().Add(15 * time.Minute).UTC(),
	})
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/obj/")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		s.puts++
		if s.putStatus != 0 {
			http.Error(w, "no", s.putStatus)
			return
		}
		b, _ := io.ReadAll(r.Body)
		s.objects[key] = b
	case http.MethodGet:
		b, ok := s.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleDownloads answers kind=snapshot with the files of the newest
// snapshot of each part before the session, like the control plane.
func (s *Server) handleDownloads(w http.ResponseWriter, r *http.Request, n int) {
	if kind := r.URL.Query().Get("kind"); kind != string(runnerproto.UploadSnapshot) {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown kind "+kind)
		return
	}
	parts := []string{"workspace", "harness_state"}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := map[string][]runnerproto.DownloadFile{}
	for k := n - 1; k >= 1 && len(found) < len(parts); k-- {
		se := s.sessions[k]
		if se == nil {
			continue
		}
		latest := map[string][]string{}
		for _, ev := range se.events {
			if ev.Type != string(runnerproto.EventSnapshotSaved) {
				continue
			}
			var d runnerproto.SnapshotSavedData
			if json.Unmarshal(ev.Data, &d) == nil {
				latest[d.Part] = d.UploadIDs
			}
		}
		for _, part := range parts {
			ids, ok := latest[part]
			if _, done := found[part]; !ok || done {
				continue
			}
			prefix := "snapshots/" + s.opts.TeamID + "/" + s.opts.RunID + "/" + strconv.Itoa(k) + "/" + part + "/"
			files := []runnerproto.DownloadFile{}
			for _, id := range ids {
				key, ok := s.ObjectKey(id)
				name, in := strings.CutPrefix(key, prefix)
				b, stored := s.objects[key]
				if !ok || !in || !stored {
					continue
				}
				files = append(files, runnerproto.DownloadFile{Part: part, Name: name, URL: s.ObjectURL(key), Bytes: int64(len(b)), ExpiresAt: time.Now().Add(15 * time.Minute).UTC()})
			}
			found[part] = files
		}
	}
	out := []runnerproto.DownloadFile{}
	for _, part := range parts {
		out = append(out, found[part]...)
	}
	writeJSON(w, http.StatusOK, runnerproto.DownloadsResponse{Files: out})
}

func (s *Server) handleGitCredential(w http.ResponseWriter, r *http.Request, _ int) {
	var req runnerproto.GitCredentialRequest
	if !s.decode(w, r, "transfer.json#/$defs/GitCredentialRequest", &req) {
		return
	}
	if s.GitCredential == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "not implemented")
		return
	}
	resp, status := s.GitCredential(req)
	if status != 0 && status != http.StatusOK {
		writeError(w, status, "forbidden", "no credential for this repository")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCloudToken(w http.ResponseWriter, r *http.Request, _ int) {
	var req runnerproto.CloudTokenRequest
	if !s.decode(w, r, "transfer.json#/$defs/CloudTokenRequest", &req) {
		return
	}
	if s.CloudToken == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", "not implemented")
		return
	}
	resp, status := s.CloudToken(req)
	if status != 0 && status != http.StatusOK {
		writeError(w, status, "forbidden", "audience not allowed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleToken(w http.ResponseWriter, _ *http.Request, n int) {
	token := randomToken("ofs_")
	s.mu.Lock()
	s.tokens[token] = n
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, runnerproto.TokenResponse{SessionToken: token, SessionTokenExpiresAt: time.Now().Add(time.Hour).UTC()})
}

// handleConversation answers with one step for each user message and the
// assistant messages after it.
func (s *Server) handleConversation(w http.ResponseWriter, _ *http.Request, _ int) {
	s.mu.Lock()
	msgs := append([]message(nil), s.messages...)
	s.mu.Unlock()
	conv := runnerproto.Conversation{ConversationID: s.opts.RunID, Steps: []runnerproto.ConversationStep{}}
	for i, m := range msgs {
		if m.role == "user" || len(conv.Steps) == 0 {
			conv.Steps = append(conv.Steps, runnerproto.ConversationStep{StepID: "step_" + strconv.Itoa(len(conv.Steps)+1), Session: 1, Messages: []runnerproto.ConversationMessage{}})
		}
		st := &conv.Steps[len(conv.Steps)-1]
		st.Messages = append(st.Messages, runnerproto.ConversationMessage{
			MessageID: "msg_" + strconv.Itoa(i+1), Role: m.role,
			Blocks: []runnerproto.Block{{Type: runnerproto.BlockText, Text: m.content}},
		})
	}
	writeJSON(w, http.StatusOK, conv)
}
