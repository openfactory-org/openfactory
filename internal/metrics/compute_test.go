package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func TestBuildCompute(t *testing.T) {
	big := &core.InstanceShape{OS: "linux", Arch: "x86_64", VCPUs: 4, MemoryGB: 8}
	d := &Data{
		Runs: []core.Run{
			{ID: "r1", AgentName: "implement", State: core.RunSucceeded, CreatedAt: at(1, 0), Usage: core.Usage{CostUSD: 2}},
			{ID: "r2", AgentName: "triage", State: core.RunSucceeded, CreatedAt: at(1, 1), Usage: core.Usage{CostUSD: 1}},
			{ID: "r3", AgentName: "triage", State: core.RunRunning, CreatedAt: at(2, 0)},
		},
		Sessions: map[string][]core.RunSession{
			// Recorded compute seconds win over the timestamps.
			"r1": {{RunID: "r1", Session: 1, InstanceShape: big, ComputeSeconds: 600, AssignedAt: at(1, 0), EndedAt: ptr(at(1, 5))},
				{RunID: "r1", Session: 2, InstanceShape: big, ComputeSeconds: 60}},
			// No shape, no recorded seconds: from the timestamps.
			"r2": {{RunID: "r2", Session: 1, AssignedAt: at(1, 1), StartedAt: ptr(at(1, 1)), EndedAt: ptr(at(1, 1).Add(2 * time.Minute))}},
			// Still open: counts until the end of the window.
			"r3": {{RunID: "r3", Session: 1, AssignedAt: until.Add(-time.Hour), StartedAt: ptr(until.Add(-time.Hour))}},
		},
	}
	db := Build("f", d, since, until)
	c := db.Cost
	if !approxEq(c.Total.ComputeSeconds, 660+120+3600) {
		t.Fatalf("total compute = %v", c.Total.ComputeSeconds)
	}
	if !approxEq(c.ByAgent["implement"].ComputeSeconds, 660) || !approxEq(c.ByAgent["triage"].ComputeSeconds, 3720) {
		t.Fatalf("by agent = %+v", c.ByAgent)
	}
	bs := c.ComputeByShape["linux/x86_64 4 vCPU 8 GB"]
	if bs.Sessions != 2 || !approxEq(bs.Seconds, 660) {
		t.Fatalf("by shape = %+v", c.ComputeByShape)
	}
	if def := c.ComputeByShape["default"]; def.Sessions != 2 || !approxEq(def.Seconds, 3720) {
		t.Fatalf("default shape = %+v", def)
	}
	if !approxEq(c.Total.CostUSD, 3) {
		t.Fatalf("inference cost changed: %v", c.Total.CostUSD)
	}
}

func TestComputeLoadsSessions(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	if err := st.CreateFactory(ctx, &core.Factory{ID: "fac", TeamID: "team1", Name: "f", CreatedAt: at(0, 0)}); err != nil {
		t.Fatal(err)
	}
	r := &core.Run{TeamID: "team1", FactoryID: "fac", AgentName: "implement", State: core.RunSucceeded, CreatedAt: at(1, 0)}
	if err := st.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	s := &core.RunSession{RunID: r.ID, Session: 1, TaskID: core.TaskID(r.ID, 1), InstanceShape: &core.InstanceShape{VCPUs: 2, MemoryGB: 4}}
	if err := st.CreateRunSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	s.State = core.SessionEnded
	s.ComputeSeconds = 125
	if err := st.UpdateRunSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	db, err := Compute(ctx, st, "fac", since, until)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Cost.ComputeByShape["2 vCPU 4 GB"]; got.Sessions != 1 || !approxEq(got.Seconds, 125) {
		t.Fatalf("by shape = %+v", db.Cost.ComputeByShape)
	}
	if !approxEq(db.Cost.Total.ComputeSeconds, 125) {
		t.Fatalf("total = %+v", db.Cost.Total)
	}
}
