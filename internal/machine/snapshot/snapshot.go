// Package snapshot builds and stores snapshot templates of microVMs. A
// template is one VM per (Firecracker version, guest kernel, tools disk,
// image digest, slot size, host CPU model, host kernel), paused at "vm-init
// ready, before the MMDS read". A restore gets the session data only after
// the restore, so clones share no secret, random seed or ID.
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/vm"
)

// KeyParts are the inputs of a template key. Any change invalidates the
// template: snapshots are not portable across Firecracker versions, CPU
// models or host kernels.
type KeyParts struct {
	FirecrackerVersion string
	KernelSHA256       string
	ToolsSHA256        string
	ImageDigest        string
	VCPUs              int
	MemoryMiB          int
	ScratchGiB         int
	HostCPUModel       string
	HostKernel         string
}

// Key returns the hex SHA-256 of the parts.
func Key(p KeyParts) string {
	s := strings.Join([]string{
		p.FirecrackerVersion, p.KernelSHA256, p.ToolsSHA256, p.ImageDigest,
		strconv.Itoa(p.VCPUs), strconv.Itoa(p.MemoryMiB), strconv.Itoa(p.ScratchGiB),
		p.HostCPUModel, p.HostKernel,
	}, "|")
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// FileSHA256 returns the hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CPUModel returns the "model name" of /proc/cpuinfo, or "CPU implementer"
// and "CPU part" of the first processor on arm64.
func CPUModel(cpuinfo string) string {
	first := map[string]string{}
	for _, line := range strings.Split(cpuinfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, seen := first[k]; !seen {
			first[k] = strings.TrimSpace(v)
		}
	}
	if m := first["model name"]; m != "" {
		return m
	}
	return first["CPU implementer"] + "/" + first["CPU part"]
}

// TemplateNet gives a template VM its isolated tap and removes it after.
type TemplateNet func(ctx context.Context) (tap string, release func(), err error)

// Manager stores templates under Dir/<key>/ and builds them one at a time.
type Manager struct {
	Dir string
	VMM vm.VMM
	Net TemplateNet
	Log *slog.Logger
	// Settle is the wait after hello before the pause (1 s): snapshots
	// taken very early in boot may crash on resume.
	Settle time.Duration

	mu       sync.Mutex
	building map[string]bool
	build    sync.Mutex
}

func (m *Manager) log() *slog.Logger {
	if m.Log == nil {
		return slog.Default()
	}
	return m.Log
}

// Get returns the files of a template, or nil when it does not exist.
func (m *Manager) Get(key string) *vm.SnapshotFiles {
	d := filepath.Join(m.Dir, key)
	f := &vm.SnapshotFiles{State: filepath.Join(d, "vm.state"), Mem: filepath.Join(d, "mem"), Scratch: filepath.Join(d, "scratch.ext4")}
	for _, p := range []string{f.State, f.Mem, f.Scratch} {
		if _, err := os.Stat(p); err != nil {
			return nil
		}
	}
	return f
}

// EnsureAsync builds the template of key in the background when it does
// not exist and no build of it runs. spec is a template spec: no image
// session data, no cache disks.
func (m *Manager) EnsureAsync(key string, spec vm.Spec) {
	if m.Get(key) != nil {
		return
	}
	m.mu.Lock()
	if m.building == nil {
		m.building = map[string]bool{}
	}
	if m.building[key] {
		m.mu.Unlock()
		return
	}
	m.building[key] = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.building, key)
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := m.Build(ctx, key, spec); err != nil {
			m.log().Warn("snapshot template build failed", "key", key, "err", err)
		}
	}()
}

// Build boots a template VM, pauses it after hello, saves the snapshot
// and the scratch disk, and removes the VM.
func (m *Manager) Build(ctx context.Context, key string, spec vm.Spec) error {
	m.build.Lock()
	defer m.build.Unlock()
	if m.Get(key) != nil {
		return nil
	}
	spec.Template = true
	spec.Cache = nil
	spec.Snapshot = nil
	spec.GuestMAC = vm.TemplateMAC
	if m.Net != nil {
		tap, release, err := m.Net(ctx)
		if err != nil {
			return err
		}
		defer release()
		spec.Tap = tap
	}
	v, err := m.VMM.Start(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = v.Cleanup(context.Background()) }()
	tv, ok := v.(vm.Template)
	if !ok {
		return errors.New("the VMM cannot snapshot")
	}
	settle := m.Settle
	if settle <= 0 {
		settle = time.Second
	}
	select {
	case <-time.After(settle):
	case <-ctx.Done():
		return ctx.Err()
	}
	files, err := tv.Snapshot(ctx)
	if err != nil {
		return err
	}
	tmp := filepath.Join(m.Dir, key+".tmp")
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	for src, name := range map[string]string{files.State: "vm.state", files.Mem: "mem", files.Scratch: "scratch.ext4"} {
		dst := filepath.Join(tmp, name)
		if err := os.Rename(src, dst); err != nil {
			if err := vm.CloneFile(src, dst); err != nil {
				return fmt.Errorf("save %s: %w", name, err)
			}
		}
		if err := os.Chmod(dst, 0o400); err != nil {
			return err
		}
		_ = os.Lchown(dst, 0, 0)
	}
	final := filepath.Join(m.Dir, key)
	_ = os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	m.log().Info("snapshot template built", "key", key)
	return nil
}

// Prune removes templates whose key keep rejects.
func (m *Manager) Prune(keep func(key string) bool) {
	entries, _ := os.ReadDir(m.Dir)
	for _, e := range entries {
		if e.IsDir() && (strings.HasSuffix(e.Name(), ".tmp") || !keep(e.Name())) {
			_ = os.RemoveAll(filepath.Join(m.Dir, e.Name()))
		}
	}
}
