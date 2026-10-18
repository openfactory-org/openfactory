package snapshot

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func sampleManifest() Manifest {
	return Manifest{
		Version:   Version,
		Part:      PartWorkspace,
		RunID:     "run_1",
		Session:   2,
		CreatedAt: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC),
		Complete:  true,
		Repositories: []RepoEntry{{
			Path: "api", Remote: "https://example.com/acme/api.git", Branch: "factory/eng-123",
			UpstreamSHA: "9f2c", HeadSHA: "41ab",
			Bundle: "repos/0.bundle", Patch: "repos/0.patch", Untracked: "repos/0.untracked.tar.zst",
		}},
		Files:   []FileEntry{{Path: "/home/agent/.config/tool/state.json", Object: "files/0", Mode: "0600"}},
		Skipped: []Skipped{{Path: "api/node_modules", Reason: "ignored"}},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := sampleManifest()
	m.CreatedAt = time.Now().UTC().Truncate(time.Second)
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, got) {
		t.Fatalf("manifest differs:\nwant %+v\ngot  %+v", m, got)
	}
}

func TestManifestJSONKeys(t *testing.T) {
	data, err := json.Marshal(sampleManifest())
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "part", "run_id", "session", "created_at", "complete", "repositories"} {
		if _, ok := top[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	var repos []map[string]json.RawMessage
	if err := json.Unmarshal(top["repositories"], &repos); err != nil || len(repos) != 1 {
		t.Fatalf("repositories: %v, %d", err, len(repos))
	}
	for _, k := range []string{"path", "remote", "branch", "upstream_sha", "head_sha", "bundle", "patch", "untracked"} {
		if _, ok := repos[0][k]; !ok {
			t.Errorf("missing repository key %q", k)
		}
	}
}

func TestManifestValidate(t *testing.T) {
	harness := &HarnessEntry{Type: "claude", SessionID: "s", Object: "harness/state.tar.zst"}
	tests := []struct {
		name string
		edit func(m *Manifest)
	}{
		{"wrong version", func(m *Manifest) { m.Version = 2 }},
		{"unknown part", func(m *Manifest) { m.Part = "other" }},
		{"empty part", func(m *Manifest) { m.Part = "" }},
		{"empty run id", func(m *Manifest) { m.RunID = "" }},
		{"session zero", func(m *Manifest) { m.Session = 0 }},
		{"workspace with harness", func(m *Manifest) { m.Harness = harness }},
		{"harness part without harness", func(m *Manifest) {
			m.Part = PartHarnessState
			m.Repositories, m.Files = nil, nil
		}},
		{"harness part with repositories", func(m *Manifest) {
			m.Part = PartHarnessState
			m.Harness = harness
			m.Files = nil
		}},
		{"harness part with files", func(m *Manifest) {
			m.Part = PartHarnessState
			m.Harness = harness
			m.Repositories = nil
		}},
		{"bundle with dotdot", func(m *Manifest) { m.Repositories[0].Bundle = "repos/../../x" }},
		{"patch absolute", func(m *Manifest) { m.Repositories[0].Patch = "/etc/passwd" }},
		{"untracked dotdot", func(m *Manifest) { m.Repositories[0].Untracked = ".." }},
		{"file object dotdot", func(m *Manifest) { m.Files[0].Object = "../files/0" }},
		{"file object absolute", func(m *Manifest) { m.Files[0].Object = "/files/0" }},
		{"harness object dotdot", func(m *Manifest) {
			m.Part = PartHarnessState
			m.Repositories, m.Files = nil, nil
			m.Harness = &HarnessEntry{Type: "claude", Object: "a/../../b"}
		}},
	}
	if err := sampleManifest().Validate(); err != nil {
		t.Fatalf("sample must be valid: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleManifest()
			tc.edit(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("expected an error")
			}
			if err := WriteManifest(t.TempDir(), m); err == nil {
				t.Fatal("WriteManifest must return the validation error")
			}
		})
	}
}

func TestWriteManifestNoTempLeft(t *testing.T) {
	dir := t.TempDir()
	if err := WriteManifest(dir, sampleManifest()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ManifestName {
		t.Fatalf("unexpected directory content: %v", entries)
	}
}
