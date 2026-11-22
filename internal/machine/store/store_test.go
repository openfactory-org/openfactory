package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

func TestPutRedactsAndLists(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	task := rtcore.Task{TaskID: "run_a.1", Env: map[string]string{rtcore.TaskEnvBootstrapToken: "ofb_secret"}}
	if err := s.Put(Record{TaskID: "run_a.1", VMID: "vm-0-aa", Task: task, Status: rtcore.TaskStatus{TaskID: "run_a.1", State: rtcore.TaskRunning}}); err != nil {
		t.Fatal(err)
	}
	if task.Env[rtcore.TaskEnvBootstrapToken] != "ofb_secret" {
		t.Fatal("Put changed the caller's task")
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir, "run_a.1.json"))
	if strings.Contains(string(data), "ofb_secret") {
		t.Fatal("the record holds the bootstrap token")
	}
	fi, _ := os.Stat(filepath.Join(s.Dir, "run_a.1.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	r, err := s.Get("run_a.1")
	if err != nil || r.VMID != "vm-0-aa" || r.Task.Env[rtcore.TaskEnvBootstrapToken] != "***" {
		t.Fatalf("get = %+v, %v", r, err)
	}
	_ = s.Put(Record{TaskID: "run_b.1"})
	list, _ := s.List()
	if len(list) != 2 || list[0].TaskID != "run_a.1" {
		t.Fatalf("list %+v", list)
	}
	if err := s.Delete("run_a.1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("run_a.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("run_a.1"); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
}

func TestBootID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "boot_id")
	_ = os.WriteFile(p, []byte("abc\n"), 0o644)
	if BootID(p) != "abc" || BootID(p+"x") != "" {
		t.Fatal("boot id")
	}
}
