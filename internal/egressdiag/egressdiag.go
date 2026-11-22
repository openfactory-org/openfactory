// Package egressdiag makes the diagnostic network/egress_blocked for a
// host that the egress proxy of a sandbox blocked. The runner uses it.
package egressdiag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/sandbox/netguard"
)

// Blocked returns the diagnostic for one blocked host of a report. The data
// has the fields blocked_host and count.
func Blocked(b netguard.BlockedHost) runnerproto.DiagnosticData {
	data, _ := json.Marshal(map[string]any{"blocked_host": b.Host, "count": b.Count})
	return runnerproto.DiagnosticData{
		Code:    netguard.DiagnosticCode,
		Message: "Network access to " + b.Host + " blocked by the network policy",
		Data:    data,
	}
}

// Fresh returns the diagnostics of the hosts of rep that are not in seen,
// and adds these hosts to seen. A host that the proxy blocks many times gives
// one diagnostic.
func Fresh(rep netguard.Report, seen map[string]bool) []runnerproto.DiagnosticData {
	var out []runnerproto.DiagnosticData
	for _, b := range rep.Blocked {
		if b.Host == "" || seen[b.Host] {
			continue
		}
		seen[b.Host] = true
		out = append(out, Blocked(b))
	}
	return out
}

// maxReport is the most bytes of a report that FetchReport reads.
const maxReport = 1 << 20

// FetchReport reads the report of blocked hosts from rawURL. The request does
// not use a proxy.
func FetchReport(ctx context.Context, rawURL string) (netguard.Report, error) {
	var rep netguard.Report
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return rep, err
	}
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return rep, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rep, fmt.Errorf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReport)).Decode(&rep); err != nil {
		return rep, err
	}
	return rep, nil
}
