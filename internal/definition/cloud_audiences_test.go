package definition

import "testing"

func TestCloudProviderAudiences(t *testing.T) {
	def := mustCompile(t, defTree("factory.yaml", factoryWith("cloudProviders:\n  audiences: [\"https://vault.example.test\", \"sts.example.test\"]\n")))
	if def.CloudProviders == nil || len(def.CloudProviders.Audiences) != 2 || def.CloudProviders.Audiences[0] != "https://vault.example.test" {
		t.Fatalf("cloudProviders = %+v", def.CloudProviders)
	}
	runErrCases(t, []errCase{
		{"duplicate audience", defTree("factory.yaml", factoryWith("cloudProviders:\n  audiences: [a, a]\n")), "factory.yaml", "cloudProviders.audiences", 0},
		{"empty audience", defTree("factory.yaml", factoryWith("cloudProviders:\n  audiences: [\"\"]\n")), "factory.yaml", "cloudProviders.audiences", 0},
	})
}
