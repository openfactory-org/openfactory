package runner_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func (e *snapEnv) addMessage(role, content string) {
	e.t.Helper()
	e.cp.AddMessage(role, content)
}

func TestConversationForSessionTwo(t *testing.T) {
	e := newSnapEnv(t)
	s1 := e.newSession(1, runnerproto.StartResponse{}, execHarness("cat"))
	s1.run()
	s1.waitReady()
	e.addMessage("user", "build the thing")
	e.addMessage("assistant", "on it")
	s1.runTurn("t1", "build the thing")
	s1.stop()

	s2 := e.newSession(2, runnerproto.StartResponse{}, execHarness("cat"))
	s2.run()
	s2.waitReady()
	s2.runTurn("t2", "continue")
	msg := s2.message()
	for _, want := range []string{
		"The harness could not resume its session. This is the conversation so far:",
		"user: build the thing", "assistant: on it", "continue",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message lacks %q: %q", want, msg)
		}
	}
	if strings.Index(msg, "assistant: on it") > strings.Index(msg, "continue") {
		t.Fatalf("the conversation must come before the input: %q", msg)
	}
	s2.stop()
}

func TestNoConversationInSessionOne(t *testing.T) {
	e := newSnapEnv(t)
	e.addMessage("user", "old")
	s1 := e.newSession(1, runnerproto.StartResponse{}, execHarness("cat"))
	s1.run()
	s1.waitReady()
	s1.runTurn("t1", "hello")
	if msg := s1.message(); strings.Contains(msg, "conversation so far") {
		t.Fatalf("session 1 got the conversation: %q", msg)
	}
	s1.stop()
}

func TestConversationUnavailable(t *testing.T) {
	e := newSnapEnv(t)
	s1 := e.newSession(1, runnerproto.StartResponse{}, execHarness("cat"))
	s1.run()
	s1.waitReady()
	s1.stop()

	s2 := e.newSession(2, runnerproto.StartResponse{}, execHarness("cat"))
	inner := e.cp.Handler()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == runnerproto.PathConversation {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	s2.cfg.ControlURL = proxy.URL
	s2.run()
	s2.waitReady()
	s2.runTurn("t2", "continue")
	if msg := s2.message(); strings.Contains(msg, "conversation so far") || !strings.Contains(msg, "continue") {
		t.Fatalf("message %q", msg)
	}
	waitFor(t, "diagnostic", func() bool { return s2.diagnostic("conversation/unavailable") })
	s2.stop()
}
