package egressdiag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openfactory-org/openfactory/internal/sandbox/netguard"
)

func TestBlocked(t *testing.T) {
	d := Blocked(netguard.BlockedHost{Host: "evil.test", Count: 4})
	if d.Code != "network/egress_blocked" || d.Message != "Network access to evil.test blocked by the network policy" {
		t.Fatalf("diagnostic = %+v", d)
	}
	var data struct {
		Host  string `json:"blocked_host"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(d.Data, &data); err != nil || data.Host != "evil.test" || data.Count != 4 {
		t.Fatalf("data = %s (%v)", d.Data, err)
	}
}

func TestFreshReportsEachHostOnce(t *testing.T) {
	seen := map[string]bool{}
	rep := netguard.Report{Blocked: []netguard.BlockedHost{{Host: "a.test", Count: 1}, {Host: "", Count: 1}, {Host: "a.test", Count: 2}, {Host: "b.test", Count: 1}}}
	if got := Fresh(rep, seen); len(got) != 2 {
		t.Fatalf("first = %d, want 2", len(got))
	}
	rep.Blocked = append(rep.Blocked, netguard.BlockedHost{Host: "c.test", Count: 1})
	if got := Fresh(rep, seen); len(got) != 1 {
		t.Fatalf("second = %d, want 1", len(got))
	}
}

func TestFetchReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != netguard.ReportPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(netguard.Report{Blocked: []netguard.BlockedHost{{Host: "a.test", Count: 1}}})
	}))
	defer srv.Close()
	rep, err := FetchReport(context.Background(), srv.URL+netguard.ReportPath)
	if err != nil || len(rep.Blocked) != 1 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if _, err := FetchReport(context.Background(), srv.URL+"/other"); err == nil {
		t.Fatal("a status that is not 200 must give an error")
	}
}
