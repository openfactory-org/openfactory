package runner

import (
	"context"
	"time"

	"github.com/openfactory-org/openfactory/internal/egressdiag"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// egressPollEvery is how often the runner reads the report of blocked hosts.
// Tests change it.
var egressPollEvery = 5 * time.Second

// watchEgress sends a diagnostic network/egress_blocked for each host that the
// proxy sidecar of the Pod blocked. The backend gives the runner the URL
// of the report in OF_NETGUARD_REPORT. The sidecar serves the report on a
// loopback address of the Pod.
//
// Without the URL, the runner does nothing. The runner
// sends one diagnostic for each host, so a host that is blocked many times
// gives one entry. A report that cannot be read is logged at debug level and
// tried again.
func (r *runner) watchEgress(ctx context.Context) {
	if r.cfg.EgressReportURL == "" {
		return
	}
	t := time.NewTicker(egressPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reportEgress(ctx)
		}
	}
}

// reportEgress reads the report once and sends the diagnostics of the hosts
// that it has not reported yet.
func (r *runner) reportEgress(ctx context.Context) {
	if r.cfg.EgressReportURL == "" {
		return
	}
	rep, err := egressdiag.FetchReport(ctx, r.cfg.EgressReportURL)
	if err != nil {
		r.log.Debug("read the egress report", "err", err)
		return
	}
	r.egressMu.Lock()
	defer r.egressMu.Unlock()
	if r.egressSeen == nil {
		r.egressSeen = map[string]bool{}
	}
	for _, d := range egressdiag.Fresh(rep, r.egressSeen) {
		r.emit(runnerproto.EventDiagnostic, "", d)
	}
}
