package runnerclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

type seen struct {
	method string
	path   string
	query  string
	header http.Header
	body   string
}

type recorder struct {
	mu   sync.Mutex
	reqs []seen
}

func (r *recorder) last(t *testing.T) seen {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		t.Fatal("server got no request")
	}
	return r.reqs[len(r.reqs)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// newServer starts a server that records each request and answers with
// the status and body.
func newServer(t *testing.T, status int, body string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: url + "/", RunnerVersion: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetToken("ofs_session")
	return c
}

func validStart() runnerproto.StartRequest {
	return runnerproto.StartRequest{
		InstanceID:    "inst",
		RunnerVersion: "1.2.3",
		Platform:      runnerproto.Platform{OS: "linux", Arch: "amd64"},
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "factory.internal", "ftp://x"} {
		if _, err := New(Options{BaseURL: u}); err == nil {
			t.Errorf("New(%q) returned no error", u)
		}
	}
}

func TestStartSetsToken(t *testing.T) {
	srv, rec := newServer(t, 200, `{"session_token":"ofs_new","run_id":"run_1"}`)
	c := newClient(t, srv.URL)
	resp, err := c.Start(context.Background(), "ofb_boot", validStart())
	if err != nil {
		t.Fatal(err)
	}
	r := rec.last(t)
	if r.method != "POST" || r.path != runnerproto.PathSessionStart {
		t.Errorf("got %s %s", r.method, r.path)
	}
	if got := r.header.Get("Authorization"); got != "Bearer ofb_boot" {
		t.Errorf("Authorization = %q", got)
	}
	if got := r.header.Get(runnerproto.HeaderRunnerVersion); got != "1.2.3" {
		t.Errorf("version header = %q", got)
	}
	if got := r.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := r.header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
	if resp.RunID != "run_1" || c.Token() != "ofs_new" {
		t.Errorf("run %q token %q", resp.RunID, c.Token())
	}
}

func TestStartValidatesRequest(t *testing.T) {
	srv, rec := newServer(t, 200, `{}`)
	c := newClient(t, srv.URL)
	req := validStart()
	req.InstanceID = ""
	if _, err := c.Start(context.Background(), "ofb_boot", req); err == nil {
		t.Fatal("no error")
	}
	if rec.count() != 0 {
		t.Errorf("server got %d requests", rec.count())
	}
}

func TestSecretsUsesSessionToken(t *testing.T) {
	srv, rec := newServer(t, 200, `{"redact":["x"]}`)
	c := newClient(t, srv.URL)
	resp, err := c.Secrets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := rec.last(t)
	if r.method != "GET" || r.path != runnerproto.PathSessionSecrets {
		t.Errorf("got %s %s", r.method, r.path)
	}
	if got := r.header.Get("Authorization"); got != "Bearer ofs_session" {
		t.Errorf("Authorization = %q", got)
	}
	if r.header.Get("Content-Type") != "" {
		t.Errorf("a request without a body has Content-Type %q", r.header.Get("Content-Type"))
	}
	if len(resp.Redact) != 1 {
		t.Errorf("redact = %v", resp.Redact)
	}
}

func oneBatch() runnerproto.EventBatch {
	return runnerproto.EventBatch{Events: []runnerproto.Event{{Seq: 1, Type: runnerproto.EventIdle}}}
}

func TestSendEventsOK(t *testing.T) {
	srv, rec := newServer(t, 200, `{"ack":1}`)
	c := newClient(t, srv.URL)
	ack, err := c.SendEvents(context.Background(), oneBatch())
	if err != nil || ack != 1 {
		t.Fatalf("ack %d err %v", ack, err)
	}
	if r := rec.last(t); r.method != "POST" || r.path != runnerproto.PathSessionEvents {
		t.Errorf("got %s %s", r.method, r.path)
	}
}

func TestSendEventsGap(t *testing.T) {
	srv, _ := newServer(t, 409, `{"code":"event_gap","ack":7}`)
	c := newClient(t, srv.URL)
	ack, err := c.SendEvents(context.Background(), oneBatch())
	if ack != 7 {
		t.Errorf("ack = %d", ack)
	}
	if !errors.Is(err, ErrGap) {
		t.Fatalf("err = %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 409 {
		t.Errorf("no StatusError 409 in %v", err)
	}
}

func TestSendEventsSuperseded(t *testing.T) {
	srv, _ := newServer(t, 409, `{"code":"session_superseded"}`)
	c := newClient(t, srv.URL)
	ack, err := c.SendEvents(context.Background(), oneBatch())
	if ack != 0 || !IsSuperseded(err) || errors.Is(err, ErrGap) {
		t.Errorf("ack %d err %v", ack, err)
	}
}

func TestCommandsQuery(t *testing.T) {
	srv, rec := newServer(t, 200, `{"commands":null}`)
	c := newClient(t, srv.URL)
	cmds, err := c.Commands(context.Background(), 5, 60)
	if err != nil {
		t.Fatal(err)
	}
	if cmds == nil || len(cmds) != 0 {
		t.Errorf("cmds = %#v", cmds)
	}
	r := rec.last(t)
	if r.method != "GET" || r.path != runnerproto.PathSessionCommands {
		t.Errorf("got %s %s", r.method, r.path)
	}
	if r.query != "after=5&wait=25" {
		t.Errorf("query = %q", r.query)
	}
}

// A poll to a replica that stopped without a reset gets no answer. The end of
// the poll deadline is a retryable error, not a context error.
func TestCommandsPollTimeoutIsRetryable(t *testing.T) {
	old := commandsGrace
	commandsGrace = 50 * time.Millisecond
	t.Cleanup(func() { commandsGrace = old })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c := newClient(t, srv.URL)

	_, err := c.Commands(context.Background(), 0, 0)
	if !errors.Is(err, ErrRequestTimeout) || !IsRetryable(err) {
		t.Errorf("poll deadline: err %v, retryable %v", err, IsRetryable(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.Commands(ctx, 0, 0)
	if errors.Is(err, ErrRequestTimeout) || IsRetryable(err) {
		t.Errorf("caller deadline: err %v, retryable %v", err, IsRetryable(err))
	}
}

// An events send to a replica that never answers ends at its own deadline
// with a retryable error, so the retry of the Sender takes over. A cancelled
// caller context is not retryable.
func TestSendEventsTimeoutIsRetryable(t *testing.T) {
	old := eventsTimeout
	eventsTimeout = 50 * time.Millisecond
	t.Cleanup(func() { eventsTimeout = old })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	c := newClient(t, srv.URL)

	start := time.Now()
	_, err := c.SendEvents(context.Background(), oneBatch())
	if !errors.Is(err, ErrRequestTimeout) || !IsRetryable(err) {
		t.Errorf("send deadline: err %v, retryable %v", err, IsRetryable(err))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the send took %v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err = c.SendEvents(ctx, oneBatch())
	if errors.Is(err, ErrRequestTimeout) || IsRetryable(err) {
		t.Errorf("cancelled caller: err %v, retryable %v", err, IsRetryable(err))
	}
}

func TestEndGap(t *testing.T) {
	srv, _ := newServer(t, 409, `{"code":"event_gap","ack":3}`)
	c := newClient(t, srv.URL)
	err := c.End(context.Background(), runnerproto.EndRequest{Reason: runnerproto.EndIdle, LastEventSeq: 9})
	if !errors.Is(err, ErrGap) {
		t.Fatalf("err = %v", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Ack == nil || *se.Ack != 3 {
		t.Errorf("StatusError = %+v", se)
	}
}

func TestEndOK(t *testing.T) {
	srv, rec := newServer(t, 200, `{}`)
	c := newClient(t, srv.URL)
	if err := c.End(context.Background(), runnerproto.EndRequest{Reason: runnerproto.EndIdle, LastEventSeq: 9}); err != nil {
		t.Fatal(err)
	}
	if r := rec.last(t); r.method != "POST" || r.path != runnerproto.PathSessionEnd {
		t.Errorf("got %s %s", r.method, r.path)
	}
	if err := c.End(context.Background(), runnerproto.EndRequest{Reason: "bad"}); err == nil {
		t.Error("invalid end request gave no error")
	}
}

func TestRefreshTokenSetsToken(t *testing.T) {
	srv, rec := newServer(t, 200, `{"session_token":"ofs_two"}`)
	c := newClient(t, srv.URL)
	if _, err := c.RefreshToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := rec.last(t)
	if r.method != "POST" || r.path != runnerproto.PathSessionToken || r.body != "" {
		t.Errorf("got %s %s body %q", r.method, r.path, r.body)
	}
	if c.Token() != "ofs_two" {
		t.Errorf("token = %q", c.Token())
	}
}

func TestPutFileNoAuthHeader(t *testing.T) {
	store, rec := newServer(t, 200, ``)
	c := newClient(t, "http://unused.invalid")
	up := runnerproto.UploadResponse{
		Method:  "PUT",
		URL:     store.URL + "/bucket/key",
		Headers: map[string]string{"X-Amz-Meta": "abc"},
	}
	data := "file bytes"
	if err := c.PutFile(context.Background(), up, strings.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	r := rec.last(t)
	if r.method != "PUT" || r.path != "/bucket/key" {
		t.Errorf("got %s %s", r.method, r.path)
	}
	if r.header.Get("Authorization") != "" || r.header.Get(runnerproto.HeaderRunnerVersion) != "" {
		t.Errorf("headers leak: %v", r.header)
	}
	if r.header.Get("X-Amz-Meta") != "abc" {
		t.Errorf("X-Amz-Meta = %q", r.header.Get("X-Amz-Meta"))
	}
	if r.body != data {
		t.Errorf("body = %q", r.body)
	}
}

func TestStatusErrorPlainBody(t *testing.T) {
	srv, _ := newServer(t, 500, "boom\n")
	c := newClient(t, srv.URL)
	_, err := c.Secrets(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 500 || se.Message != "boom" {
		t.Errorf("err = %v", err)
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"503", &StatusError{Status: 503}, true},
		{"400", &StatusError{Status: 400}, false},
		{"401", &StatusError{Status: 401}, false},
		{"422 configuration error", &StatusError{Status: 422, Code: runnerproto.CodeConfigurationError}, false},
		{"500", &StatusError{Status: 500, Code: "internal"}, true},
		{"network", errors.New("dial"), true},
	}
	for _, tt := range tests {
		if got := IsRetryable(tt.err); got != tt.want {
			t.Errorf("%s: IsRetryable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDownloadsQuery(t *testing.T) {
	srv, rec := newServer(t, 200, `{"files":[]}`)
	c := newClient(t, srv.URL)
	if _, err := c.Downloads(context.Background(), "attachment", "att_1"); err != nil {
		t.Fatal(err)
	}
	if q := rec.last(t).query; q != "id=att_1&kind=attachment" {
		t.Errorf("query = %q", q)
	}
	if _, err := c.Downloads(context.Background(), "snapshot", ""); err != nil {
		t.Fatal(err)
	}
	if q := rec.last(t).query; q != "kind=snapshot" {
		t.Errorf("query = %q", q)
	}
}

func TestRunCancelled(t *testing.T) {
	srv, _ := newServer(t, 410, `{"code":"run_cancelled"}`)
	c := newClient(t, srv.URL)
	_, err := c.Secrets(context.Background())
	if !IsRunCancelled(err) {
		t.Errorf("err = %v", err)
	}
}
