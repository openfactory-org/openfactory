package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Version is the format version of the manifest.
	Version = 1
	// PartWorkspace is the part that holds repositories and files.
	PartWorkspace = "workspace"
	// PartHarnessState is the part that holds the session files of the harness.
	PartHarnessState = "harness_state"
	// ManifestName is the file name of the manifest in a part directory.
	ManifestName = "manifest.json"
)

// Manifest describes one snapshot part.
type Manifest struct {
	Version      int           `json:"version"`
	Part         string        `json:"part"`
	RunID        string        `json:"run_id"`
	Session      int           `json:"session"`
	CreatedAt    time.Time     `json:"created_at"`
	Complete     bool          `json:"complete"`
	Repositories []RepoEntry   `json:"repositories,omitempty"`
	Files        []FileEntry   `json:"files,omitempty"`
	Harness      *HarnessEntry `json:"harness,omitempty"`
	Skipped      []Skipped     `json:"skipped,omitempty"`
}

// RepoEntry describes the saved state of one git repository.
type RepoEntry struct {
	Path        string `json:"path"`
	Remote      string `json:"remote"`
	Branch      string `json:"branch"`
	UpstreamSHA string `json:"upstream_sha"`
	HeadSHA     string `json:"head_sha"`
	Bundle      string `json:"bundle,omitempty"`
	Patch       string `json:"patch,omitempty"`
	Untracked   string `json:"untracked,omitempty"`
}

// FileEntry describes one file that is saved as it is.
type FileEntry struct {
	Path   string `json:"path"`
	Object string `json:"object"`
	Mode   string `json:"mode"`
}

// HarnessEntry describes the saved session files of a harness.
type HarnessEntry struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Object    string `json:"object"`
}

// Skipped names something that the snapshot does not contain, and why.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// WriteManifest validates m and writes it to dir/manifest.json. It writes a
// temporary file in dir first and then renames it, so a reader never sees a
// partial file.
func WriteManifest(dir string, m Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot: encode manifest: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, ManifestName+".tmp-*")
	if err != nil {
		return fmt.Errorf("snapshot: create temporary manifest: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("snapshot: write manifest: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("snapshot: set manifest mode: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("snapshot: close manifest: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, ManifestName)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("snapshot: rename manifest: %w", err)
	}
	return nil
}

// ReadManifest reads dir/manifest.json and validates it.
func ReadManifest(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("snapshot: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("snapshot: decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks the rules of the manifest format.
func (m Manifest) Validate() error {
	if m.Version != Version {
		return fmt.Errorf("snapshot: unsupported manifest version %d", m.Version)
	}
	switch m.Part {
	case PartWorkspace:
		if m.Harness != nil {
			return errors.New("snapshot: a workspace part must not have a harness entry")
		}
	case PartHarnessState:
		if m.Harness == nil {
			return errors.New("snapshot: a harness_state part must have a harness entry")
		}
		if len(m.Repositories) > 0 || len(m.Files) > 0 {
			return errors.New("snapshot: a harness_state part must not have repositories or files")
		}
	default:
		return fmt.Errorf("snapshot: unknown part %q", m.Part)
	}
	if m.RunID == "" {
		return errors.New("snapshot: run_id is empty")
	}
	if m.Session < 1 {
		return fmt.Errorf("snapshot: session must be 1 or more, got %d", m.Session)
	}
	for i, r := range m.Repositories {
		for _, f := range []struct{ name, value string }{
			{"bundle", r.Bundle}, {"patch", r.Patch}, {"untracked", r.Untracked},
		} {
			if err := checkRelative(f.value); err != nil {
				return fmt.Errorf("snapshot: repositories[%d].%s: %w", i, f.name, err)
			}
		}
	}
	for i, f := range m.Files {
		if err := checkRelative(f.Object); err != nil {
			return fmt.Errorf("snapshot: files[%d].object: %w", i, err)
		}
	}
	if m.Harness != nil {
		if err := checkRelative(m.Harness.Object); err != nil {
			return fmt.Errorf("snapshot: harness.object: %w", err)
		}
	}
	return nil
}

// checkRelative accepts an empty value or a relative path without a ".."
// element and without a leading slash.
func checkRelative(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q is absolute", p)
	}
	for _, el := range strings.Split(p, "/") {
		if el == ".." {
			return fmt.Errorf("path %q contains a .. element", p)
		}
	}
	return nil
}
