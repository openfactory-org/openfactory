package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// frozenClock keeps the turn wall time at 0, so the runner adds no duration
// of its own.
func frozenClock() time.Time { return time.Unix(1700000000, 0) }

// stepClock returns a clock that moves step forward on each call.
func stepClock(step time.Duration) func() time.Time {
	var mu sync.Mutex
	t := time.Unix(1700000000, 0)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(step)
		return t
	}
}

// TestCoreHarnessUsageTotals checks the session totals over turns: tokens
// and cost add up, the duration is the reported one or else the turn wall
// time, and a partial turn makes the totals partial.
func TestCoreHarnessUsageTotals(t *testing.T) {
	turns := []struct {
		name   string
		cancel bool // the turn is interrupted before its events
		events []rtcore.HarnessEvent
		want   runnerproto.UsageData // last usage event of the turn
	}{
		{
			name: "reported duration",
			events: []rtcore.HarnessEvent{
				{Kind: "usage", Usage: &rtcore.Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.5, DurationMS: 4000}},
				{Kind: "done", Content: "a", Usage: &rtcore.Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.5, DurationMS: 4000}},
			},
			want: runnerproto.UsageData{InputTokens: 10, OutputTokens: 2, CostUSD: 0.5, DurationMS: 4000, Cumulative: true, Coverage: "known"},
		},
		{
			name:   "no reported duration uses wall time",
			events: []rtcore.HarnessEvent{{Kind: "done", Content: "b", Usage: &rtcore.Usage{InputTokens: 5, OutputTokens: 1}}},
			want:   runnerproto.UsageData{InputTokens: 15, OutputTokens: 3, CostUSD: 0.5, DurationMS: 5000, Cumulative: true, Coverage: "known"},
		},
		{
			name:   "interrupted turn keeps partial usage",
			cancel: true,
			events: []rtcore.HarnessEvent{
				{Kind: "message", Content: "late"},
				{Kind: "usage", Usage: &rtcore.Usage{InputTokens: 100, OutputTokens: 7, Coverage: rtcore.UsagePartial}},
				{Kind: "error", Content: "claude: context canceled"},
			},
			want: runnerproto.UsageData{InputTokens: 115, OutputTokens: 10, CostUSD: 0.5, DurationMS: 6000, Cumulative: true, Coverage: "partial"},
		},
		{
			name:   "partial stays after a known turn",
			events: []rtcore.HarnessEvent{{Kind: "usage", Usage: &rtcore.Usage{InputTokens: 1, DurationMS: 500}}, {Kind: "done", Content: "d"}},
			want:   runnerproto.UsageData{InputTokens: 116, OutputTokens: 10, CostUSD: 0.5, DurationMS: 6500, Cumulative: true, Coverage: "partial"},
		},
	}
	n := 0
	var cancel context.CancelFunc
	f := &fakeCoreHarness{script: func(int, rtcore.HarnessRequest) []rtcore.HarnessEvent {
		if turns[n].cancel {
			cancel()
		}
		return turns[n].events
	}}
	// Each clock read moves 1s, so a turn without a reported duration has 1s
	// of wall time.
	c := &CoreHarness{H: f, now: stepClock(time.Second)}
	for i, tc := range turns {
		n = i
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		var cp capture
		res := c.Turn(ctx, baseTurnRequest(), cp.emit)
		cancel()
		if tc.cancel && res.Outcome != runnerproto.OutcomeInterrupted {
			t.Fatalf("%s: outcome %+v", tc.name, res)
		}
		us := cp.all(runnerproto.EventUsage)
		if len(us) == 0 {
			t.Fatalf("%s: no usage event", tc.name)
		}
		if got := unmarshal[runnerproto.UsageData](t, us[len(us)-1]); got != tc.want {
			t.Fatalf("%s: usage %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
