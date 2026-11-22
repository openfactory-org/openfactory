// Package store keeps the task records of a worker host as JSON files. A
// record holds Task.Redacted(), never the bootstrap token.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// Record is the persistent state of one task.
type Record struct {
	TaskID      string            `json:"task_id"`
	VMID        string            `json:"vm_id"`
	Slot        int               `json:"slot"`
	Status      rtcore.TaskStatus `json:"status"`
	ImageDigest string            `json:"image_digest,omitempty"`
	BootID      string            `json:"boot_id"`
	// StreamKey lets a restarted agent accept the reconnect of vm-init.
	StreamKey string `json:"stream_key,omitempty"`
	// Spec is the VM spec, kept so that a restarted agent can adopt it.
	Spec json.RawMessage `json:"spec,omitempty"`
	// Net is the network mode ("restricted" or "unrestricted").
	Net string `json:"net,omitempty"`
	// CacheKeys are the git cache disks of the VM.
	CacheKeys []string `json:"cache_keys,omitempty"`
	// HelperPID is the network helper of the VM. A restarted agent stops
	// it and starts its own.
	HelperPID int         `json:"helper_pid,omitempty"`
	Deadline  time.Time   `json:"deadline"`
	Task      rtcore.Task `json:"task"`
}

// Store is a directory of records.
type Store struct {
	Dir string
}

// New creates the directory with mode 0700.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) path(taskID string) string {
	return filepath.Join(s.Dir, strings.ReplaceAll(taskID, "/", "_")+".json")
}

// Put writes a record atomically with mode 0600. It redacts the task.
func (s *Store) Put(r Record) error {
	r.Task = r.Task.Redacted()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	p := s.path(r.TaskID)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Get reads one record.
func (s *Store) Get(taskID string) (Record, error) {
	var r Record
	data, err := os.ReadFile(s.path(taskID))
	if errors.Is(err, os.ErrNotExist) {
		return r, fmt.Errorf("task %s: %w", taskID, rtcore.ErrNotFound)
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(data, &r)
}

// Delete removes a record. A missing record is no error.
func (s *Store) Delete(taskID string) error {
	err := os.Remove(s.path(taskID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns all records sorted by task ID. Unreadable files are skipped.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(data, &r) == nil && r.TaskID != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out, nil
}

// BootID reads the boot ID of the host from path
// (/proc/sys/kernel/random/boot_id). It returns "" when it cannot.
func BootID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
