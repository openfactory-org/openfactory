package runner

import (
	"context"
	"reflect"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func TestCoreHarnessMergesMCPEnv(t *testing.T) {
	f := &fakeCoreHarness{script: func(int, rtcore.HarnessRequest) []rtcore.HarnessEvent {
		return []rtcore.HarnessEvent{{Kind: "done"}}
	}}
	c := &CoreHarness{H: f}
	req := baseTurnRequest()
	req.Harness.MCPServers = map[string]runnerproto.MCPServer{
		"local-tool": {Command: "tool", Env: map[string]string{"A": "1"}},
		"other":      {Command: "other"},
	}
	req.MCPEnv = map[string]map[string]string{"local-tool": {"TOOL_TOKEN": "secret"}}
	var cp capture
	if res := c.Turn(context.Background(), req, cp.emit); res.Outcome != runnerproto.OutcomeDone {
		t.Fatalf("result %+v", res)
	}
	got := f.reqs[0].MCPServers
	if want := map[string]string{"A": "1", "TOOL_TOKEN": "secret"}; !reflect.DeepEqual(got["local-tool"].Env, want) {
		t.Fatalf("env %v, want %v", got["local-tool"].Env, want)
	}
	if got["other"].Env != nil {
		t.Fatalf("other env %v", got["other"].Env)
	}
	if len(req.Harness.MCPServers["local-tool"].Env) != 1 {
		t.Fatal("the start response was changed")
	}
}
