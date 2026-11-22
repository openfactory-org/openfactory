package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/sandbox/netguard"
)

func egressRunner(t *testing.T, url string) *runner {
	t.Helper()
	log, err := runnerclient.OpenEventLog(filepath.Join(t.TempDir(), "events.jsonl"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return &runner{
		cfg: Config{EgressReportURL: url}, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		events: log, flushCh: make(chan struct{}, 1),
	}
}

func diagnostics(t *testing.T, r *runner) []runnerproto.DiagnosticData {
	t.Helper()
	evs, err := r.events.After(0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var out []runnerproto.DiagnosticData
	for _, ev := range evs {
		if ev.Type != runnerproto.EventDiagnostic {
			continue
		}
		var d runnerproto.DiagnosticData
		if err := ev.Decode(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// TestRunnerReportsBlockedHostsAsDiagnostics checks that a host that the
// egress proxy blocked gives one diagnostic network/egress_blocked with
// data.blocked_host, also when the proxy blocks it many times.
func TestRunnerReportsBlockedHostsAsDiagnostics(t *testing.T) {
	rep := netguard.Report{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != netguard.ReportPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(rep)
	}))
	defer srv.Close()
	r := egressRunner(t, srv.URL+netguard.ReportPath)
	ctx := context.Background()

	r.reportEgress(ctx)
	if got := diagnostics(t, r); len(got) != 0 {
		t.Fatalf("diagnostics without a blocked host: %v", got)
	}
	rep.Blocked = []netguard.BlockedHost{{Host: "evil.example", Count: 3}}
	r.reportEgress(ctx)
	// More blocks of the same host and one new host: one new diagnostic.
	rep.Blocked = []netguard.BlockedHost{{Host: "evil.example", Count: 9}, {Host: "other.example", Count: 1}}
	r.reportEgress(ctx)
	r.reportEgress(ctx)

	got := diagnostics(t, r)
	if len(got) != 2 {
		t.Fatalf("diagnostics = %+v", got)
	}
	for i, want := range []string{"evil.example", "other.example"} {
		if got[i].Code != "network/egress_blocked" {
			t.Errorf("code = %q", got[i].Code)
		}
		var data struct {
			Host  string `json:"blocked_host"`
			Count int    `json:"count"`
		}
		if err := json.Unmarshal(got[i].Data, &data); err != nil || data.Host != want || data.Count < 1 {
			t.Errorf("data = %s, err = %v", got[i].Data, err)
		}
	}
}

func TestEgressReportProblemsAreIgnored(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	for _, url := range []string{"", bad.URL, "http://127.0.0.1:1/blocked"} {
		r := egressRunner(t, url)
		r.reportEgress(context.Background())
		if got := diagnostics(t, r); len(got) != 0 {
			t.Errorf("url %q: diagnostics = %v", url, got)
		}
	}
}

func TestWatchEgressPollsUntilStopped(t *testing.T) {
	old := egressPollEvery
	egressPollEvery = 10 * time.Millisecond
	t.Cleanup(func() { egressPollEvery = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(netguard.Report{Blocked: []netguard.BlockedHost{{Host: "x.example", Count: 1}}})
	}))
	defer srv.Close()
	r := egressRunner(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.watchEgress(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(diagnostics(t, r)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got := diagnostics(t, r); len(got) != 1 {
		t.Fatalf("diagnostics = %v", got)
	}
}
