package dispatch

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// TestUsageDurationAndCoverage: the run usage is the sum of the cumulative
// usage of its sessions, with the duration, and is partial when one session
// is partial.
func TestUsageDurationAndCoverage(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e)
	s := e.session(run.ID, 1)

	steps := []struct {
		name string
		data runnerproto.UsageData
		want core.Usage
	}{
		{
			name: "old runner without duration",
			data: runnerproto.UsageData{InputTokens: 10, OutputTokens: 5, CostUSD: 0.5, Cumulative: true, Coverage: "known"},
			want: core.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: 0.5},
		},
		{
			name: "duration",
			data: runnerproto.UsageData{InputTokens: 20, OutputTokens: 6, CostUSD: 0.75, DurationMS: 4000, Cumulative: true, Coverage: "known"},
			want: core.Usage{InputTokens: 20, OutputTokens: 6, CostUSD: 0.75, DurationMS: 4000},
		},
		{
			name: "partial",
			data: runnerproto.UsageData{InputTokens: 30, OutputTokens: 8, CostUSD: 0.75, DurationMS: 9000, Cumulative: true, Coverage: "partial"},
			want: core.Usage{InputTokens: 30, OutputTokens: 8, CostUSD: 0.75, DurationMS: 9000, Coverage: core.UsagePartial},
		},
	}
	for i, st := range steps {
		e.events(s, event(t, int64(i+1), runnerproto.EventUsage, turn, st.data))
		if got := e.run(run.ID).Usage; got != st.want {
			t.Fatalf("%s: run usage %+v, want %+v", st.name, got, st.want)
		}
		if got := e.session(run.ID, 1).Usage; got != st.want {
			t.Fatalf("%s: session usage %+v, want %+v", st.name, got, st.want)
		}
	}

	// A second session adds to the run; a known session does not clear
	// the partial coverage of the first.
	e.endSessionRaw(run.ID, 1)
	s2 := &core.RunSession{RunID: run.ID, Session: 2, State: core.SessionActive, BootstrapExpiresAt: *e.clock, AssignedAt: *e.clock}
	if err := e.mem.CreateRunSession(e.ctx, s2); err != nil {
		t.Fatal(err)
	}
	e.events(e.session(run.ID, 2), event(t, 1, runnerproto.EventUsage, turn,
		runnerproto.UsageData{InputTokens: 1, OutputTokens: 1, CostUSD: 0.25, DurationMS: 1000, Cumulative: true, Coverage: "known"}))
	want := core.Usage{InputTokens: 31, OutputTokens: 9, CostUSD: 1, DurationMS: 10000, Coverage: core.UsagePartial}
	if got := e.run(run.ID).Usage; got != want {
		t.Fatalf("two sessions: run usage %+v, want %+v", got, want)
	}
}
