package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/machine/cache"
	"github.com/openfactory-org/openfactory/internal/machine/config"
	"github.com/openfactory-org/openfactory/internal/machine/image"
	"github.com/openfactory-org/openfactory/internal/machine/snapshot"
	"github.com/openfactory-org/openfactory/internal/machine/store"
	"github.com/openfactory-org/openfactory/internal/machine/vmnet"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

type fakeCtl struct {
	mu       sync.Mutex
	draining bool
}

func (f *fakeCtl) Status(context.Context) Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Status{WorkerID: "wkr_1", Draining: f.draining, Slots: 2}
}

func (f *fakeCtl) SetDraining(d bool) {
	f.mu.Lock()
	f.draining = d
	f.mu.Unlock()
}

func TestControlSocket(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "ofa")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "agent.sock")
	stop, err := ServeControl(sock, &fakeCtl{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v", fi.Mode())
	}
	c := NewClient(sock)
	st, err := c.Status(context.Background())
	if err != nil || st.WorkerID != "wkr_1" || st.Draining {
		t.Fatalf("status %+v %v", st, err)
	}
	st, err = c.Drain(context.Background(), true)
	if err != nil || !st.Draining {
		t.Fatalf("drain %+v %v", st, err)
	}
	if _, err := NewClient(filepath.Join(dir, "none.sock")).Status(context.Background()); err == nil {
		t.Fatal("want an error without an agent")
	}
}

func TestNetConfigAndBase(t *testing.T) {
	c := config.Default()
	c.Network.InternalAllow = []string{"10.1.2.3:443"}
	n := NetConfig(c, "nameserver 127.0.0.53\n")
	if n.Subnet.String() != "10.213.0.0/16" || len(n.Resolvers) != 1 || len(n.InternalAllow) != 1 || !n.BlockSMTP || n.MaxConns != 2048 {
		t.Fatalf("net config %+v", n)
	}
	st, _ := store.New(t.TempDir())
	rec := &vmnet.RecordingApplier{}
	if err := applyBase(context.Background(), rec, n, st); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.Base[0], "table inet openfactory\ndelete table") {
		t.Fatalf("full apply expected:\n%s", rec.Base[0])
	}
	_ = st.Put(store.Record{TaskID: "run_a.1", Status: rtcore.TaskStatus{State: rtcore.TaskRunning}})
	if err := applyBase(context.Background(), rec, n, st); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Base[1], "delete table") || !strings.Contains(rec.Base[1], "flush chain") {
		t.Fatalf("refresh expected:\n%s", rec.Base[1])
	}
	tap, release, err := templateNet(rec, n, c)(context.Background())
	if err != nil || tap != "oftpl0" {
		t.Fatal(err)
	}
	added, _ := rec.Snapshot()
	if !strings.Contains(added["oftpl0"], "tpl_in drop") {
		t.Fatalf("template rules %q", added["oftpl0"])
	}
	release()
}

func TestOfflineStatus(t *testing.T) {
	c := config.Default()
	c.DataDir = t.TempDir()
	if _, err := OfflineStatus(c); err == nil {
		t.Fatal("not enrolled")
	}
}

// pruneDisk removes the snapshot templates of images that are no longer cached.
func TestPruneDiskSnapshots(t *testing.T) {
	c := config.Default()
	c.DataDir = t.TempDir()
	c.Disk.ImagesMaxGiB, c.Disk.GitcacheMaxGiB = 100, 100
	images := &image.Store{Dir: c.DataDir}
	digest := "sha256:" + strings.Repeat("a", 64)
	dir := images.DigestDir(digest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte("ext4"), 0o444)
	_ = os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"digest":"`+digest+`"}`), 0o644)
	snaps := &snapshot.Manager{Dir: filepath.Join(c.DataDir, "snapshots")}
	for _, k := range []string{"key-" + digest, "key-sha256:gone"} {
		if err := os.MkdirAll(filepath.Join(snaps.Dir, k), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var tools string
	snapKey := func(img image.Image, t string) string { tools = t; return "key-" + img.Digest }
	gc := &cache.Cache{Dir: filepath.Join(c.DataDir, "gitcache")}
	pruneDisk(c, images, gc, snaps, snapKey, "1.2.3", "amd64", func(string) bool { return false }, slog.Default())
	if _, err := os.Stat(filepath.Join(snaps.Dir, "key-"+digest)); err != nil {
		t.Fatal("pruned the template of a cached image")
	}
	if _, err := os.Stat(filepath.Join(snaps.Dir, "key-sha256:gone")); err == nil {
		t.Fatal("kept the template of an image that is gone")
	}
	if !strings.HasSuffix(tools, "tools/1.2.3-amd64/tools.ext4") {
		t.Fatalf("tools path %q", tools)
	}
}
