package objectstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRunDataCategoriesAreIndependent(t *testing.T) {
	clk := newClock()
	def, arts := newMemStore(clk.Now), newMemStore(clk.Now)
	rd, err := NewRunData(def, nil, arts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := rd.PutHarnessTranscript(ctx, "r1", 1, strings.NewReader("{}\n"), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.PutArtifact(ctx, "r1", "shots/1.png", strings.NewReader("png"), 3, "image/png"); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.PutAttachment(ctx, "r1", "spec.md", strings.NewReader("# x"), 3, "text/markdown"); err != nil {
		t.Fatal(err)
	}
	if got := arts.keys(); !slices.Equal(got, []string{"runs/r1/artifacts/shots/1.png"}) {
		t.Fatalf("artifact store keys %v", got)
	}
	dk := def.keys()
	if len(dk) != 2 || dk[0] != "runs/r1/attachments/spec.md" || dk[1] != "runs/r1/harness/1.jsonl" {
		t.Fatalf("default store keys %v", dk)
	}
	if rd.Store(Artifacts) != Store(arts) || rd.Store(Transcripts) != Store(def) || rd.Store("x") != nil {
		t.Fatal("Store(category) mapping wrong")
	}
	if _, err := NewRunData(nil, def, nil, def, nil); err == nil {
		t.Fatal("NewRunData without default and a nil category succeeded")
	}
}

func TestRunDataArtifactsAndAttachments(t *testing.T) {
	fs, srv, clk, _ := newFileFixture(t)
	rd, _ := NewRunData(fs, nil, nil, nil, nil)
	rd.SetClock(clk.Now)
	ctx := context.Background()
	for _, n := range []string{"b.txt", "a/z.log"} {
		if _, err := rd.PutArtifact(ctx, "r1", n, strings.NewReader(n), int64(len(n)), "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = rd.PutArtifact(ctx, "r2", "other.txt", strings.NewReader("x"), 1, "")
	arts, err := rd.ListArtifacts(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 || arts[0].Key != "a/z.log" || arts[1].Key != "b.txt" {
		t.Fatalf("artifacts %+v", arts)
	}
	u, err := rd.ArtifactURL(ctx, "r1", "b.txt", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Get(u)
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp.Body); resp.StatusCode != 200 || body != "b.txt" {
		t.Fatalf("artifact URL: %d %q", resp.StatusCode, body)
	}
	rc, _, err := rd.OpenArtifact(ctx, "r1", "a/z.log")
	if err != nil || readAll(t, rc) != "a/z.log" {
		t.Fatalf("OpenArtifact: %v", err)
	}

	// Attachment uploaded directly by a client through a presigned URL.
	up, err := rd.AttachmentUploadURL(ctx, "r1", "prompt.png", time.Minute, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, up, strings.NewReader("PNG"))
	for k, v := range PutHeaders(rd.Store(Attachments), "image/png") {
		req.Header[k] = v
	}
	if resp, err := srv.Client().Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("attachment upload: %v %v", resp, err)
	}
	atts, _ := rd.ListAttachments(ctx, "r1")
	if len(atts) != 1 || atts[0].Key != "prompt.png" || atts[0].ContentType != "image/png" {
		t.Fatalf("attachments %+v", atts)
	}
	if _, err := rd.AttachmentURL(ctx, "r1", "prompt.png", time.Minute); err != nil {
		t.Fatal(err)
	}
	rc, _, err = rd.OpenAttachment(ctx, "r1", "prompt.png")
	if err != nil || readAll(t, rc) != "PNG" {
		t.Fatalf("OpenAttachment: %v", err)
	}

	for _, bad := range []struct{ run, name string }{{"", "x"}, {"a/b", "x"}, {"..", "x"}, {"r1", "../../r2/artifacts/other.txt"}, {"r1", "/etc/passwd"}} {
		if _, err := rd.PutArtifact(ctx, bad.run, bad.name, strings.NewReader("x"), 1, ""); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("PutArtifact(%q,%q) = %v", bad.run, bad.name, err)
		}
		if _, err := rd.ArtifactURL(ctx, bad.run, bad.name, time.Minute); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ArtifactURL(%q,%q) = %v", bad.run, bad.name, err)
		}
	}

	if err := rd.DeleteRun(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if l, _ := fs.List(ctx, "runs/r1/"); len(l) != 0 {
		t.Fatalf("r1 objects left: %+v", l)
	}
	if l, _ := fs.List(ctx, "runs/r2/"); len(l) != 1 {
		t.Fatalf("r2 objects touched: %+v", l)
	}
}

func TestRunDataPrune(t *testing.T) {
	clk := newClock()
	tr, arts := newMemStore(clk.Now), newMemStore(clk.Now)
	rd, _ := NewRunData(tr, nil, arts, nil, nil)
	rd.SetClock(clk.Now)
	ctx := context.Background()
	old := clk.Now().Add(-40 * 24 * time.Hour)

	// old: everything old. mixed: old transcript, fresh artifact. fresh: new.
	for _, run := range []string{"old", "mixed", "fresh"} {
		k, _ := rd.PutHarnessTranscript(ctx, run, 1, strings.NewReader("{}"), 2)
		if run != "fresh" {
			tr.setMod(k, old)
		}
		ak, _ := rd.PutArtifact(ctx, run, "a.bin", strings.NewReader("x"), 1, "")
		if run == "old" {
			arts.setMod(ak, old)
		}
	}
	// Objects that are not run data are left alone.
	_ = tr.Put(ctx, "other/thing", strings.NewReader("x"), 1, "")
	tr.setMod("other/thing", old)

	pruned, err := rd.Prune(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pruned, []string{"old"}) {
		t.Fatalf("pruned %v", pruned)
	}
	for _, k := range append(tr.keys(), arts.keys()...) {
		if strings.HasPrefix(k, "runs/old/") {
			t.Errorf("%s survived", k)
		}
	}
	if len(arts.keys()) != 2 || !slices.Contains(tr.keys(), "other/thing") {
		t.Fatalf("unexpected deletions: tr=%v arts=%v", tr.keys(), arts.keys())
	}
	if _, err := rd.Prune(ctx, 0); err == nil {
		t.Fatal("zero retention accepted")
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := ConfigFromEnv(env(map[string]string{"OF_DATA_DIR": "/srv/of"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != "file:///srv/of/objects" || c.PresignTTL != DefaultPresignTTL || c.Retention != 0 {
		t.Fatalf("defaults %+v", c)
	}
	c, _ = ConfigFromEnv(env(map[string]string{"HOME": "/home/u"}))
	if c.URL != "file:///home/u/.openfactory/objects" {
		t.Fatalf("home default %s", c.URL)
	}
	c, err = ConfigFromEnv(env(map[string]string{
		EnvURL: "s3://b", EnvArtifactsURL: "gs://g", EnvTranscriptsURL: "azblob://a/c", EnvAttachmentsURL: "file:///x",
		EnvRetention: "720h", EnvPresignTTL: "5m", EnvSigningKey: "sk",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{URL: "s3://b", ArtifactsURL: "gs://g", TranscriptsURL: "azblob://a/c", AttachmentsURL: "file:///x",
		Retention: 720 * time.Hour, PresignTTL: 5 * time.Minute, SigningKey: []byte("sk")}
	if fmt.Sprint(c) != fmt.Sprint(want) {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
	for _, bad := range []map[string]string{{EnvRetention: "forever"}, {EnvPresignTTL: "-1m"}} {
		bad["OF_DATA_DIR"] = "/x"
		if _, err := ConfigFromEnv(env(bad)); err == nil {
			t.Errorf("ConfigFromEnv(%v) succeeded", bad)
		}
	}
}

func TestConfigOpenRunData(t *testing.T) {
	base := t.TempDir()
	c := Config{
		URL:          "file://" + filepath.ToSlash(filepath.Join(base, "default")),
		ArtifactsURL: "file://" + filepath.ToSlash(filepath.Join(base, "arts")),
		SigningKey:   []byte("sk"),
	}
	rd, stores, err := c.OpenRunData(context.Background(), Options{BaseURL: "https://of.example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stores) != 2 {
		t.Fatalf("opened %d stores, want 2 (deduplicated)", len(stores))
	}
	if rd.Store(Transcripts) != rd.Store(Attachments) || rd.Store(Artifacts) == rd.Store(Transcripts) {
		t.Fatal("category mapping wrong")
	}
	u, err := rd.ArtifactURL(context.Background(), "r", "a.txt", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://of.example/objects-artifacts/runs/r/artifacts/a.txt?") {
		t.Fatalf("artifact URL %s", u)
	}
	u, _ = rd.AttachmentURL(context.Background(), "r", "a.txt", time.Minute)
	if !strings.HasPrefix(u, "https://of.example/objects/runs/r/attachments/a.txt?") {
		t.Fatalf("attachment URL %s", u)
	}
	mux := http.NewServeMux()
	hs := FileHandlers(stores)
	if len(hs) != 2 {
		t.Fatalf("%d handlers", len(hs))
	}
	for _, h := range hs {
		h.Register(mux) // must not panic on conflicting patterns
	}
	if _, _, err := (Config{URL: "bogus://x"}).OpenRunData(context.Background(), Options{}); err == nil {
		t.Fatal("bad URL accepted")
	}
}

// Artifacts collected from a sandbox keep their directory layout in the name.
func TestRunDataNestedArtifactNames(t *testing.T) {
	ctx := context.Background()
	rd, err := NewRunData(newMemStore(newClock().Now), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.PutArtifact(ctx, "run_1", "data/out.json", strings.NewReader("{}"), 2, "application/json"); err != nil {
		t.Fatal(err)
	}
	objs, err := rd.ListArtifacts(ctx, "run_1")
	if err != nil || len(objs) != 1 || objs[0].Key != "data/out.json" {
		t.Fatalf("list = %+v, %v", objs, err)
	}
	rc, _, err := rd.OpenArtifact(ctx, "run_1", "data/out.json")
	if err != nil || readAll(t, rc) != "{}" {
		t.Fatalf("open: %v", err)
	}
	if _, err := rd.PutArtifact(ctx, "run_1", "../escape", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("escaping name accepted")
	}
}

// Snapshot objects live under snapshots/{team}/{run}/ and follow the
// retention of the run.
func TestRunDataPruneSnapshots(t *testing.T) {
	clk := newClock()
	def, snaps := newMemStore(clk.Now), newMemStore(clk.Now)
	rd, err := NewRunData(def, nil, nil, nil, snaps)
	if err != nil {
		t.Fatal(err)
	}
	rd.SetClock(clk.Now)
	if rd.Store(Snapshots) != Store(snaps) {
		t.Fatal("Store(Snapshots) mapping wrong")
	}
	ctx := context.Background()
	old := clk.Now().Add(-40 * 24 * time.Hour)
	for _, k := range []string{"snapshots/default/old/1/workspace/a.tar", "snapshots/default/fresh/1/workspace/a.tar"} {
		if err := snaps.Put(ctx, k, strings.NewReader("x"), 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	snaps.setMod("snapshots/default/old/1/workspace/a.tar", old)
	pruned, err := rd.Prune(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pruned, []string{"old"}) {
		t.Fatalf("pruned %v", pruned)
	}
	if got := snaps.keys(); !slices.Equal(got, []string{"snapshots/default/fresh/1/workspace/a.tar"}) {
		t.Fatalf("snapshot keys %v", got)
	}
	if err := rd.DeleteRun(ctx, "fresh"); err != nil {
		t.Fatal(err)
	}
	if got := snaps.keys(); len(got) != 0 {
		t.Fatalf("DeleteRun left %v", got)
	}
}

// pruneAttachmentsCase checks PruneAttachments on a RunData whose artifacts
// store is st. age moves the last-modified time of a key by d into the past.
func pruneAttachmentsCase(t *testing.T, rd *RunData, st Store, clk *clock, age func(key string, d time.Duration)) {
	t.Helper()
	ctx := context.Background()
	put := func(key string) {
		if err := st.Put(ctx, key, strings.NewReader("x"), 1, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	// Two files of one old attachment, one fresh attachment, a run artifact
	// and an object that is not run data.
	oldKeys := []string{"attachments/team_1/att_old/a.txt", "attachments/team_1/att_old/b.txt"}
	for _, k := range oldKeys {
		put(k)
		age(k, 40*24*time.Hour)
	}
	put("attachments/team_1/att_new/c.txt")
	age("attachments/team_1/att_new/c.txt", 2*24*time.Hour)
	put("runs/run_x/artifacts/old.bin")
	age("runs/run_x/artifacts/old.bin", 40*24*time.Hour)
	put("other/thing")
	age("other/thing", 40*24*time.Hour)

	if _, err := rd.PruneAttachments(ctx, 0); err == nil {
		t.Fatal("zero retention accepted")
	}
	ids, err := rd.PruneAttachments(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"att_old"}) {
		t.Fatalf("pruned %v", ids)
	}
	list, err := st.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range list {
		got = append(got, o.Key)
	}
	slices.Sort(got)
	want := []string{"attachments/team_1/att_new/c.txt", "other/thing", "runs/run_x/artifacts/old.bin"}
	if !slices.Equal(got, want) {
		t.Fatalf("keys after prune %v, want %v", got, want)
	}
	// A second run finds nothing to do.
	if ids, err := rd.PruneAttachments(ctx, 30*24*time.Hour); err != nil || len(ids) != 0 {
		t.Fatalf("second prune: %v %v", ids, err)
	}
}

func TestRunDataPruneAttachmentsMemory(t *testing.T) {
	clk := newClock()
	st := newMemStore(clk.Now)
	rd, err := NewRunData(st, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rd.SetClock(clk.Now)
	pruneAttachmentsCase(t, rd, st, clk, func(key string, d time.Duration) { st.setMod(key, clk.Now().Add(-d)) })
}

// The local file store takes the last-modified time from the file.
func TestRunDataPruneAttachmentsLocal(t *testing.T) {
	fs, _, clk, dir := newFileFixture(t)
	rd, err := NewRunData(fs, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rd.SetClock(clk.Now)
	pruneAttachmentsCase(t, rd, fs, clk, func(key string, d time.Duration) {
		mt := clk.Now().Add(-d)
		if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(key)), mt, mt); err != nil {
			t.Fatal(err)
		}
	})
}

// With S3 the fake server keeps the times in its own store, and lists in
// pages (page size 2), so the prune must follow the pages.
func TestRunDataPruneAttachmentsS3(t *testing.T) {
	s3, _, clk, f := newS3Fixture(t, true, "")
	rd, err := NewRunData(s3, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rd.SetClock(clk.Now)
	pruneAttachmentsCase(t, rd, s3, clk, func(key string, d time.Duration) { f.store.setMod(key, clk.Now().Add(-d)) })
}
