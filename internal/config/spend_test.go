package config

import "testing"

func TestTeamSpendLimit(t *testing.T) {
	c, err := LoadFrom(nil, env(map[string]string{"OF_DEV": "1", "OF_DATA_DIR": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if d, m := c.TeamSpendLimitUSD(); d != 0 || m != 0 {
		t.Fatalf("default: %v %v", d, m)
	}
	c, err = LoadFrom(nil, env(map[string]string{"OF_DEV": "1", "OF_DATA_DIR": t.TempDir(),
		"OF_TEAM_SPEND_LIMIT_DAILY": "12.5", "OF_TEAM_SPEND_LIMIT_MONTHLY": "$300"}))
	if err != nil {
		t.Fatal(err)
	}
	if d, m := c.TeamSpendLimitUSD(); d != 12.5 || m != 300 {
		t.Fatalf("values: %v %v", d, m)
	}
	for _, bad := range []map[string]string{
		{"OF_TEAM_SPEND_LIMIT_DAILY": "-1"},
		{"OF_TEAM_SPEND_LIMIT_MONTHLY": "lots"},
		{"OF_TEAM_SPEND_LIMIT_MONTHLY": "NaN"},
	} {
		bad["OF_DEV"], bad["OF_DATA_DIR"] = "1", t.TempDir()
		if _, err := LoadFrom(nil, env(bad)); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
}
