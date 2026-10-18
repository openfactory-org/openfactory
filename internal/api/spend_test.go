package api

import (
	"context"
	"testing"
)

func TestFactorySpend(t *testing.T) {
	var gotTeam, gotFactory string
	e := newTestEnv(t, func(o *Options) {
		o.Spend = func(_ context.Context, teamID, factoryID string) (any, error) {
			gotTeam, gotFactory = teamID, factoryID
			return map[string]any{"factory_id": factoryID, "factory_daily_usd": 1.5,
				"limits": []map[string]any{{"scope": "factory", "period": "daily", "spent_usd": 1.5, "limit_usd": 2}}}, nil
		}
	})
	fid := e.createFactory("web")
	r := e.must(e.do("GET", "/factories/"+fid+"/spend", e.viewer.Key, nil), 200)
	if gotFactory != fid || gotTeam != e.team.ID || r.Body["factory_daily_usd"] != 1.5 {
		t.Fatalf("spend = %s (team %q factory %q)", r.Raw, gotTeam, gotFactory)
	}
	e.must(e.do("GET", "/factories/"+fid+"/spend", e.other.Key, nil), 404)

	plain := newTestEnv(t)
	pf := plain.createFactory("web")
	if r := plain.do("GET", "/factories/"+pf+"/spend", plain.viewer.Key, nil); r.Status == 200 {
		t.Fatal("without a spend source the route must not answer 200")
	}
}
