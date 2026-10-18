package definition

import "testing"

func TestAllowAgentMerge(t *testing.T) {
	if plain := mustCompile(t, defTree("factory.yaml", factoryWith(""))); plain.AllowAgentMerge {
		t.Fatal("allowAgentMerge must default to false")
	}
	def := mustCompile(t, defTree("factory.yaml", factoryWith("allowAgentMerge: true\n")))
	if !def.AllowAgentMerge {
		t.Fatal("allowAgentMerge not compiled")
	}
	if _, errs := Compile(defTree("factory.yaml", factoryWith("allowAgentMerge: sometimes\n"))); len(errs) == 0 {
		t.Fatal("a value that is not a boolean must fail")
	}
}

func TestSpendLimit(t *testing.T) {
	if plain := mustCompile(t, defTree("factory.yaml", factoryWith(""))); plain.SpendLimit != nil {
		t.Fatal("no spendLimit means no limit")
	}
	def := mustCompile(t, defTree("factory.yaml", factoryWith("spendLimit:\n  daily: 25\n  monthly: 400.5\n")))
	if def.SpendLimit == nil || def.SpendLimit.DailyUSD != 25 || def.SpendLimit.MonthlyUSD != 400.5 {
		t.Fatalf("spendLimit = %+v", def.SpendLimit)
	}
	def = mustCompile(t, defTree("factory.yaml", factoryWith("spendLimit:\n  monthly: 10\n")))
	if def.SpendLimit == nil || def.SpendLimit.DailyUSD != 0 || def.SpendLimit.MonthlyUSD != 10 {
		t.Fatalf("spendLimit = %+v", def.SpendLimit)
	}
	for _, bad := range []string{
		"spendLimit:\n  daily: 0\n",
		"spendLimit:\n  daily: -5\n",
		"spendLimit:\n  weekly: 5\n",
		"spendLimit:\n  daily: lots\n",
	} {
		if _, errs := Compile(defTree("factory.yaml", factoryWith(bad))); len(errs) == 0 {
			t.Errorf("%q must fail", bad)
		}
	}
}
