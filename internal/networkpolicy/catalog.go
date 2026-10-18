// Package networkpolicy implements outbound network policies for factory runs:
// the built-in policy catalog (builtin/package-registries, builtin/claude-code,
// builtin/codex) with versioned host lists, version selectors, host validation,
// and the factory+agent merge with deny-wins semantics.
package networkpolicy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// BuiltinPrefix is the namespace of built-in policies.
const BuiltinPrefix = "builtin/"

// Policy is one built-in policy and its published versions, as returned by
// GET /api/v1/networking/policies.
type Policy struct {
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	LatestVersion string          `json:"latest_version"`
	Versions      []PolicyVersion `json:"versions"`
}

// PolicyVersion is one immutable version of a built-in policy.
type PolicyVersion struct {
	Version      string   `json:"version"`
	IsLatest     bool     `json:"is_latest"`
	AllowedHosts []string `json:"allowed_hosts"`
}

type builtinVersion struct {
	version string
	hosts   []string
}

type builtinPolicy struct {
	name        string
	description string
	versions    []builtinVersion // any order; sorted on use
}

var packageRegistries100 = []string{
	// JavaScript
	"registry.npmjs.org",
	"registry.yarnpkg.com",
	"repo.yarnpkg.com",
	"nodejs.org",
	// Python
	"pypi.org",
	"files.pythonhosted.org",
	"pypi.python.org",
	// Rust
	"crates.io",
	"index.crates.io",
	"static.crates.io",
	"static.rust-lang.org",
	"sh.rustup.rs",
	// Ruby
	"rubygems.org",
	"index.rubygems.org",
	// Go
	"proxy.golang.org",
	"sum.golang.org",
	"go.dev",
	"dl.google.com",
	// JVM
	"repo.maven.apache.org",
	"repo1.maven.org",
	"plugins.gradle.org",
	"services.gradle.org",
	"downloads.gradle.org",
	// .NET / PHP / Elixir
	"api.nuget.org",
	"packagist.org",
	"repo.packagist.org",
	"hex.pm",
	"repo.hex.pm",
	// Source and release downloads
	"github.com",
	"codeload.github.com",
	"objects.githubusercontent.com",
	"raw.githubusercontent.com",
	"release-assets.githubusercontent.com",
}

var packageRegistries110 = append(append([]string{}, packageRegistries100...),
	"jsr.io",
	"npm.jsr.io",
	"bun.sh",
	"deno.land",
	"dl.deno.land",
	"astral.sh",
	"conda.anaconda.org",
	"repo.anaconda.com",
	"gitlab.com",
)

var claudeCode100 = []string{
	"api.anthropic.com",
	"console.anthropic.com",
	"statsig.anthropic.com",
}

var claudeCode110 = append(append([]string{}, claudeCode100...),
	"claude.ai",
	"platform.claude.com",
)

var codex100 = []string{
	"api.openai.com",
	"auth.openai.com",
	"chatgpt.com",
}

var builtins = []builtinPolicy{
	{
		name:        BuiltinPrefix + "package-registries",
		description: "Package registries and source or release downloads used by development toolchains.",
		versions: []builtinVersion{
			{"1.0.0", packageRegistries100},
			{"1.1.0", packageRegistries110},
		},
	},
	{
		name:        BuiltinPrefix + "claude-code",
		description: "Inference and authentication services for Claude Code.",
		versions: []builtinVersion{
			{"1.0.0", claudeCode100},
			{"1.1.0", claudeCode110},
		},
	},
	{
		name:        BuiltinPrefix + "codex",
		description: "Inference and authentication services for Codex.",
		versions: []builtinVersion{
			{"1.0.0", codex100},
		},
	},
}

// Catalog returns every built-in policy with its versions sorted ascending.
// The catalog excludes platform-required service allowances.
func Catalog() []Policy {
	out := make([]Policy, 0, len(builtins))
	for _, b := range builtins {
		vs := sortedVersions(b)
		latest := latestStable(vs)
		p := Policy{Name: b.name, Description: b.description, LatestVersion: latest.String()}
		for _, v := range vs {
			bv := findVersion(b, v.String())
			hosts := append([]string(nil), bv.hosts...)
			sort.Strings(hosts)
			p.Versions = append(p.Versions, PolicyVersion{
				Version:      v.String(),
				IsLatest:     v.String() == p.LatestVersion,
				AllowedHosts: hosts,
			})
		}
		out = append(out, p)
	}
	return out
}

// Resolve resolves a reference of the form "<policy-name>@<version-selector>"
// and returns the resolved version string and its allowed hosts.
//
// Selectors: an exact version ("1.0.0"), a caret range ("^1", "^1.1", "^1.0.0")
// selecting the highest matching stable version, or "latest" selecting the
// highest stable version.
func Resolve(ref string) (version string, hosts []string, err error) {
	name, sel, ok := strings.Cut(strings.TrimSpace(ref), "@")
	if !ok || name == "" || sel == "" {
		return "", nil, fmt.Errorf("policy reference %q must have the form <policy-name>@<version-selector>", ref)
	}
	var bp *builtinPolicy
	for i := range builtins {
		if builtins[i].name == name {
			bp = &builtins[i]
			break
		}
	}
	if bp == nil {
		return "", nil, fmt.Errorf("unknown built-in policy %q (available: %s)", name, strings.Join(policyNames(), ", "))
	}
	vs := sortedVersions(*bp)
	var chosen *semver
	switch {
	case sel == "latest":
		v := latestStable(vs)
		chosen = &v
	case strings.HasPrefix(sel, "^"):
		lo, hi, err := caretRange(sel[1:])
		if err != nil {
			return "", nil, fmt.Errorf("policy reference %q: %w", ref, err)
		}
		for i := len(vs) - 1; i >= 0; i-- {
			v := vs[i]
			if v.pre == "" && !v.less(lo) && v.less(hi) {
				chosen = &vs[i]
				break
			}
		}
	default:
		want, err := parseSemver(sel)
		if err != nil {
			return "", nil, fmt.Errorf("policy reference %q: invalid version selector %q (use an exact version, ^range, or latest)", ref, sel)
		}
		for i := range vs {
			if vs[i].equal(want) {
				chosen = &vs[i]
				break
			}
		}
	}
	if chosen == nil {
		return "", nil, fmt.Errorf("policy reference %q matches no published version of %s", ref, name)
	}
	bv := findVersion(*bp, chosen.String())
	return chosen.String(), append([]string(nil), bv.hosts...), nil
}

func policyNames() []string {
	names := make([]string, len(builtins))
	for i, b := range builtins {
		names[i] = b.name
	}
	return names
}

func findVersion(b builtinPolicy, v string) builtinVersion {
	for _, bv := range b.versions {
		if bv.version == v {
			return bv
		}
	}
	return builtinVersion{}
}

func sortedVersions(b builtinPolicy) []semver {
	vs := make([]semver, 0, len(b.versions))
	for _, bv := range b.versions {
		v, err := parseSemver(bv.version)
		if err != nil {
			panic("networkpolicy: invalid built-in version " + bv.version)
		}
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i].less(vs[j]) })
	return vs
}

func latestStable(vs []semver) semver {
	for i := len(vs) - 1; i >= 0; i-- {
		if vs[i].pre == "" {
			return vs[i]
		}
	}
	return vs[len(vs)-1]
}

// semver is a minimal semantic version (MAJOR.MINOR.PATCH[-PRERELEASE]).
type semver struct {
	major, minor, patch int
	pre                 string
}

func (v semver) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
	if v.pre != "" {
		s += "-" + v.pre
	}
	return s
}

func (v semver) equal(o semver) bool { return v == o }

func (v semver) less(o semver) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	if v.patch != o.patch {
		return v.patch < o.patch
	}
	// A pre-release sorts before the release.
	switch {
	case v.pre == o.pre:
		return false
	case v.pre == "":
		return false
	case o.pre == "":
		return true
	default:
		return v.pre < o.pre
	}
}

func parseSemver(s string) (semver, error) {
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid version %q", s)
	}
	nums, err := parseNums(parts)
	if err != nil {
		return semver{}, fmt.Errorf("invalid version %q", s)
	}
	if strings.Contains(s, "-") && pre == "" {
		return semver{}, fmt.Errorf("invalid version %q", s)
	}
	return semver{nums[0], nums[1], nums[2], pre}, nil
}

func parseNums(parts []string) ([]int, error) {
	out := make([]int, len(parts))
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return nil, fmt.Errorf("invalid number %q", p)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid number %q", p)
		}
		out[i] = n
	}
	return out, nil
}

// caretRange returns [lo, hi) for a caret selector body such as "1", "1.2" or "1.2.3".
func caretRange(s string) (lo, hi semver, err error) {
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return lo, hi, fmt.Errorf("invalid range ^%s", s)
	}
	nums, err := parseNums(parts)
	if err != nil {
		return lo, hi, fmt.Errorf("invalid range ^%s", s)
	}
	for len(nums) < 3 {
		nums = append(nums, 0)
	}
	lo = semver{major: nums[0], minor: nums[1], patch: nums[2]}
	switch {
	case nums[0] > 0 || len(parts) == 1:
		hi = semver{major: nums[0] + 1}
	case nums[1] > 0 || len(parts) == 2:
		hi = semver{minor: nums[1] + 1}
	default:
		hi = semver{patch: nums[2] + 1}
	}
	return lo, hi, nil
}
