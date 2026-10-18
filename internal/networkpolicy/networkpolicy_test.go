package networkpolicy

import (
	"slices"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestResolveSelectors(t *testing.T) {
	tests := []struct {
		ref     string
		want    string
		wantErr string
	}{
		{"builtin/package-registries@1.0.0", "1.0.0", ""},
		{"builtin/package-registries@1.1.0", "1.1.0", ""},
		{"builtin/package-registries@^1", "1.1.0", ""},
		{"builtin/package-registries@^1.0", "1.1.0", ""},
		{"builtin/package-registries@^1.1.0", "1.1.0", ""},
		{"builtin/package-registries@latest", "1.1.0", ""},
		{"builtin/claude-code@^1", "1.1.0", ""},
		{"builtin/codex@latest", "1.0.0", ""},
		{"builtin/codex@1.0.0", "1.0.0", ""},
		{"builtin/codex@^2", "", "matches no published version"},
		{"builtin/codex@9.9.9", "", "matches no published version"},
		{"builtin/codex@1.0", "", "invalid version selector"},
		{"builtin/codex", "", "must have the form"},
		{"builtin/unknown@1.0.0", "", "unknown built-in policy"},
		{"other/package-registries@1.0.0", "", "unknown built-in policy"},
		{"builtin/codex@^x", "", "invalid range"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			v, hosts, err := Resolve(tt.ref)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Resolve(%q) err = %v, want %q", tt.ref, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.ref, err)
			}
			if v != tt.want {
				t.Errorf("version = %s, want %s", v, tt.want)
			}
			if len(hosts) == 0 {
				t.Error("no hosts")
			}
		})
	}
}

func TestCaretRangeZeroMajor(t *testing.T) {
	lo, hi, err := caretRange("0.2")
	if err != nil {
		t.Fatal(err)
	}
	if lo.String() != "0.2.0" || hi.String() != "0.3.0" {
		t.Errorf("got [%s,%s)", lo, hi)
	}
	lo, hi, _ = caretRange("0.0.3")
	if lo.String() != "0.0.3" || hi.String() != "0.0.4" {
		t.Errorf("got [%s,%s)", lo, hi)
	}
}

func TestSemverOrdering(t *testing.T) {
	a, _ := parseSemver("1.0.0-rc.1")
	b, _ := parseSemver("1.0.0")
	if !a.less(b) || b.less(a) {
		t.Error("pre-release must sort before release")
	}
	if _, err := parseSemver("01.0.0"); err == nil {
		t.Error("leading zero accepted")
	}
	if latestStable([]semver{b, {major: 2, pre: "beta"}}) != b {
		t.Error("latest must skip pre-releases")
	}
}

func TestCatalog(t *testing.T) {
	cat := Catalog()
	if len(cat) != 3 {
		t.Fatalf("catalog has %d policies", len(cat))
	}
	for _, p := range cat {
		if !strings.HasPrefix(p.Name, BuiltinPrefix) {
			t.Errorf("policy %s lacks builtin prefix", p.Name)
		}
		latest := 0
		for _, v := range p.Versions {
			if v.IsLatest {
				latest++
				if v.Version != p.LatestVersion {
					t.Errorf("%s: is_latest on %s but latest_version %s", p.Name, v.Version, p.LatestVersion)
				}
			}
			if !slices.IsSorted(v.AllowedHosts) {
				t.Errorf("%s@%s hosts not sorted", p.Name, v.Version)
			}
			for _, h := range v.AllowedHosts {
				if n, err := NormalizeHost(h); err != nil || n != h {
					t.Errorf("%s@%s: invalid host %q", p.Name, v.Version, h)
				}
			}
		}
		if latest != 1 {
			t.Errorf("%s: %d latest versions", p.Name, latest)
		}
	}
	// Spot checks for realistic host lists.
	_, pr, _ := Resolve("builtin/package-registries@1.0.0")
	for _, h := range []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "crates.io", "rubygems.org", "proxy.golang.org", "repo.maven.apache.org", "objects.githubusercontent.com"} {
		if !slices.Contains(pr, h) {
			t.Errorf("package-registries missing %s", h)
		}
	}
	_, cc, _ := Resolve("builtin/claude-code@1.0.0")
	if !slices.Contains(cc, "api.anthropic.com") {
		t.Error("claude-code missing api.anthropic.com")
	}
	_, cx, _ := Resolve("builtin/codex@1.0.0")
	if !slices.Contains(cx, "api.openai.com") {
		t.Error("codex missing api.openai.com")
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		in, want, err string
	}{
		{"API.Example.com", "api.example.com", ""},
		{"api.example.com.", "api.example.com", ""},
		{"10.0.0.1", "10.0.0.1", ""},
		{"2001:db8::1", "2001:db8::1", ""},
		{"localhost", "localhost", ""},
		{"https://api.example.com", "", "not a URL"},
		{"api.example.com/path", "", "not a URL"},
		{"api.example.com:443", "", "port"},
		{"[2001:db8::1]:443", "", "brackets"},
		{"10.0.0.0/8", "", "CIDR"},
		{"*.example.com", "", "wildcards"},
		{"", "", "empty"},
		{"bad host", "", "not a valid hostname"},
		{"-bad.example.com", "", "not a valid hostname"},
		{"a..b", "", "not a valid hostname"},
		{strings.Repeat("a", 64) + ".com", "", "not a valid hostname"},
	}
	for _, tt := range tests {
		got, err := NormalizeHost(tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("NormalizeHost(%q) err = %v, want %q", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestMerge(t *testing.T) {
	factory := &core.NetworkPolicy{
		Extends:      []string{"builtin/codex@1.0.0"},
		AllowedHosts: []string{"api.example.com"},
		DeniedHosts:  []string{"blocked.example.com", "chatgpt.com"},
	}
	agent := &core.NetworkPolicy{AllowedHosts: []string{"Docs.Example.com", "blocked.example.com"}}

	t.Run("unrestricted when neither scope declares", func(t *testing.T) {
		got, err := Merge(nil, nil)
		if err != nil || got.Restricted {
			t.Fatalf("got %+v, %v", got, err)
		}
		if !Allowed(got, "anything.example.org") {
			t.Error("unrestricted policy blocked host")
		}
	})

	t.Run("allowances combine and denials win", func(t *testing.T) {
		got, err := Merge(factory, agent)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Restricted {
			t.Fatal("expected restricted")
		}
		for host, want := range map[string]bool{
			"api.example.com":       true,
			"docs.example.com":      true,
			"api.openai.com":        true,
			"auth.openai.com:443":   true,
			"chatgpt.com":           false, // extends host denied at factory scope
			"blocked.example.com":   false, // agent cannot re-allow
			"other.example.com":     false,
			"sub.api.example.com":   false, // exact match only
			"https://api.example":   false,
			"API.EXAMPLE.COM":       true,
			"docs.example.com:8080": true,
		} {
			if Allowed(got, host) != want {
				t.Errorf("Allowed(%q) = %v, want %v", host, !want, want)
			}
		}
		if slices.Contains(got.AllowedHosts, "blocked.example.com") {
			t.Error("denied host present in allowed list")
		}
	})

	t.Run("empty factory policy denies all except agent allowances", func(t *testing.T) {
		got, err := Merge(&core.NetworkPolicy{}, nil)
		if err != nil || !got.Restricted || len(got.AllowedHosts) != 0 {
			t.Fatalf("got %+v, %v", got, err)
		}
		got, _ = Merge(&core.NetworkPolicy{}, &core.NetworkPolicy{AllowedHosts: []string{"x.example.com"}})
		if !Allowed(got, "x.example.com") {
			t.Error("agent allowance dropped")
		}
		// An empty agent policy does not cancel factory allowances.
		got, _ = Merge(&core.NetworkPolicy{AllowedHosts: []string{"y.example.com"}}, &core.NetworkPolicy{})
		if !Allowed(got, "y.example.com") {
			t.Error("factory allowance cancelled by empty agent policy")
		}
	})

	t.Run("deny-only policy still restricts", func(t *testing.T) {
		got, _ := Merge(nil, &core.NetworkPolicy{DeniedHosts: []string{"bad.example.com"}})
		if !got.Restricted || Allowed(got, "good.example.com") {
			t.Errorf("deny-only policy acted as denylist: %+v", got)
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		_, err := Merge(&core.NetworkPolicy{AllowedHosts: []string{"*.x.com"}}, &core.NetworkPolicy{Extends: []string{"builtin/nope@1"}})
		if err == nil || !strings.Contains(err.Error(), "factory networkPolicy") || !strings.Contains(err.Error(), "agent networkPolicy") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestWithRequired(t *testing.T) {
	p, _ := Merge(&core.NetworkPolicy{DeniedHosts: []string{"github.com"}}, nil)
	if Allowed(p, "github.com") {
		t.Fatal("github.com should be denied before required hosts")
	}
	p2 := WithRequired(p, "github.com:443", "control.example.com")
	if !Allowed(p2, "github.com") || !Allowed(p2, "control.example.com") {
		t.Errorf("required hosts blocked: %+v", p2)
	}
	if Allowed(p, "github.com") {
		t.Error("WithRequired mutated input")
	}
	un := WithRequired(core.EffectiveNetworkPolicy{}, "x.com")
	if un.Restricted || len(un.AllowedHosts) != 0 {
		t.Error("unrestricted policy changed")
	}
}
