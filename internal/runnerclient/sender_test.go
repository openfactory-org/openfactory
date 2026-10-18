package runnerclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// fakeControlPlane plays the events endpoint. It keeps the events that it got.
type fakeControlPlane struct {
	mu       sync.Mutex
	have     []int64 // sequence numbers, in order
	requests int
	failures int    // answer 503 this many times first
	status   int    // if not 0, always answer with this status
	body     string // body for status
}

func (f *fakeControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	w.Header().Set("Content-Type", "application/json")
	if f.failures > 0 {
		f.failures--
		w.WriteHeader(503)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		w.Write([]byte(f.body))
		return
	}
	var b runnerproto.EventBatch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || len(b.Events) == 0 {
		w.WriteHeader(400)
		return
	}
	ack := int64(len(f.have))
	if b.Events[0].Seq > ack+1 {
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(runnerproto.ErrorBody{Code: "event_gap", Ack: &ack})
		return
	}
	for _, e := range b.Events {
		if e.Seq == int64(len(f.have))+1 {
			f.have = append(f.have, e.Seq)
		}
	}
	json.NewEncoder(w).Encode(runnerproto.EventAck{Ack: int64(len(f.have))})
}

type sleepRecorder struct {
	d   []time.Duration
	err error
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.d = append(s.d, d)
	return s.err
}

func newSender(t *testing.T, cp *fakeControlPlane, events int, batch int) (*Sender, *EventLog, *sleepRecorder) {
	t.Helper()
	srv := httptest.NewServer(cp)
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL)
	l, _ := openLog(t)
	appendN(t, l, events)
	sl := &sleepRecorder{}
	s, err := NewSender(SenderOptions{Client: c, Log: l, BatchBytes: batch, Sleep: sl.sleep})
	if err != nil {
		t.Fatal(err)
	}
	return s, l, sl
}

func checkHave(t *testing.T, cp *fakeControlPlane, n int) {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.have) != n {
		t.Fatalf("server has %d events, want %d", len(cp.have), n)
	}
	for i, s := range cp.have {
		if s != int64(i+1) {
			t.Fatalf("server events = %v", cp.have)
		}
	}
}

func TestNewSenderNil(t *testing.T) {
	if _, err := NewSender(SenderOptions{}); err == nil {
		t.Error("no error")
	}
}

func TestFlushSendsAll(t *testing.T) {
	cp := &fakeControlPlane{}
	s, _, _ := newSender(t, cp, 5, 0)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkHave(t, cp, 5)
	if s.Acked() != 5 {
		t.Errorf("Acked = %d", s.Acked())
	}
}

func TestFlushNothingToSend(t *testing.T) {
	cp := &fakeControlPlane{}
	s, _, _ := newSender(t, cp, 0, 0)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cp.requests != 0 {
		t.Errorf("requests = %d", cp.requests)
	}
}

func TestFlushInBatches(t *testing.T) {
	cp := &fakeControlPlane{}
	s, _, _ := newSender(t, cp, 12, 400)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cp.requests < 2 {
		t.Errorf("requests = %d", cp.requests)
	}
	checkHave(t, cp, 12)
}

func TestFlushRetriesOn503(t *testing.T) {
	cp := &fakeControlPlane{failures: 2}
	s, _, sl := newSender(t, cp, 2, 0)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second}
	if len(sl.d) != 2 || sl.d[0] != want[0] || sl.d[1] != want[1] {
		t.Errorf("sleeps = %v", sl.d)
	}
	checkHave(t, cp, 2)
}

// A send that gets no answer ends at its own deadline, and Flush sends again
// (a connection to a replica that stopped without a reset).
func TestFlushRetriesAfterASendTimeout(t *testing.T) {
	old := eventsTimeout
	eventsTimeout = 50 * time.Millisecond
	t.Cleanup(func() { eventsTimeout = old })
	cp := &fakeControlPlane{}
	var mu sync.Mutex
	hung := false
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := !hung
		hung = true
		mu.Unlock()
		if first {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		cp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	l, _ := openLog(t)
	appendN(t, l, 3)
	sl := &sleepRecorder{}
	s, err := NewSender(SenderOptions{Client: newClient(t, srv.URL), Log: l, Sleep: sl.sleep})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sl.d) != 1 || sl.d[0] != time.Second {
		t.Errorf("sleeps = %v", sl.d)
	}
	checkHave(t, cp, 3)
}

func TestFlushBackoffCap(t *testing.T) {
	cp := &fakeControlPlane{failures: 7}
	s, _, sl := newSender(t, cp, 1, 0)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	if len(sl.d) != len(want) {
		t.Fatalf("sleeps = %v", sl.d)
	}
	for i, w := range want {
		if sl.d[i] != w*time.Second {
			t.Errorf("sleep %d = %v", i, sl.d[i])
		}
	}
}

func TestFlushGapResends(t *testing.T) {
	cp := &fakeControlPlane{have: []int64{1, 2, 3}}
	s, l, sl := newSender(t, cp, 5, 0)
	s.SetAcked(5)
	appendN(t, l, 2)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkHave(t, cp, 7)
	if s.Acked() != 7 {
		t.Errorf("Acked = %d", s.Acked())
	}
	if len(sl.d) != 0 {
		t.Errorf("sleeps = %v", sl.d)
	}
}

func TestFlushStopsOnSuperseded(t *testing.T) {
	cp := &fakeControlPlane{status: 409, body: `{"code":"session_superseded"}`}
	s, _, sl := newSender(t, cp, 1, 0)
	err := s.Flush(context.Background())
	if !IsSuperseded(err) {
		t.Errorf("err = %v", err)
	}
	if len(sl.d) != 0 {
		t.Errorf("sleeps = %v", sl.d)
	}
}

func TestFlushContextCancelled(t *testing.T) {
	cp := &fakeControlPlane{failures: 1}
	s, _, sl := newSender(t, cp, 1, 0)
	sl.err = context.Canceled
	if err := s.Flush(context.Background()); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestFlushSameGapTwice(t *testing.T) {
	cp := &fakeControlPlane{status: 409, body: `{"code":"event_gap","ack":0}`}
	s, _, _ := newSender(t, cp, 1, 0)
	if err := s.Flush(context.Background()); !errors.Is(err, ErrGap) {
		t.Errorf("err = %v", err)
	}
}
