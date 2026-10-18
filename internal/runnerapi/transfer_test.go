package runnerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/objectstore"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

// fakeFiles is a FileStore that records the keys and makes fake URLs.
type fakeFiles struct {
	mu      sync.Mutex
	puts    []string
	gets    []string
	objects map[string]int64 // existing objects and their sizes
}

func (f *fakeFiles) PresignPut(_ context.Context, key string, _ time.Duration, ct string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, key)
	return "https://files.test/put/" + key, nil
}

func (f *fakeFiles) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, key)
	return "https://files.test/get/" + key, nil
}

func (f *fakeFiles) Stat(_ context.Context, key string) (objectstore.ObjectInfo, error) {
	n, ok := f.objects[key]
	if !ok {
		return objectstore.ObjectInfo{}, objectstore.ErrNotFound
	}
	return objectstore.ObjectInfo{Key: key, Size: n}, nil
}

const sha = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// call sends a raw request with a session token for the given session.
func (e *env) call(method, path string, session int, body any) (int, []byte) {
	e.t.Helper()
	tok, _, err := runnertoken.Mint(e.keys, runnertoken.Claims{
		Version: runnertoken.SessionVersion, Team: e.run.TeamID, Run: e.run.ID, Session: session, Subject: "runner",
	}, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	var rd bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = *bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, &rd)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func TestUploadKeyLayout(t *testing.T) {
	ff := &fakeFiles{}
	e := newEnv(t, func(c *Config) { c.Files = ff })
	run := e.run.ID
	cases := []struct {
		req  runnerproto.UploadRequest
		want string
	}{
		{runnerproto.UploadRequest{Kind: "artifact", Name: "out/a.txt", Bytes: 5, SHA256: sha, ContentType: "text/plain"},
			"runs/" + run + "/sessions/1/artifact/out/a.txt"},
		{runnerproto.UploadRequest{Kind: "transcript", Name: "t.jsonl", Bytes: 5, SHA256: sha},
			"runs/" + run + "/sessions/1/transcript/t.jsonl"},
		{runnerproto.UploadRequest{Kind: "snapshot", Part: "workspace", Name: "src/x.go", Bytes: 5, SHA256: sha},
			"snapshots/team_1/" + run + "/1/workspace/src/x.go"},
		{runnerproto.UploadRequest{Kind: "snapshot", Part: "harness_state", Name: "s.json", Bytes: 5, SHA256: sha},
			"snapshots/team_1/" + run + "/1/harness_state/s.json"},
	}
	for _, tc := range cases {
		code, body := e.call("POST", runnerproto.PathSessionUploads, 1, tc.req)
		if code != 200 {
			t.Fatalf("%v: %d %s", tc.req, code, body)
		}
		var resp runnerproto.UploadResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.URL != "https://files.test/put/"+tc.want || resp.Method != "PUT" || resp.ExpiresAt.IsZero() {
			t.Errorf("resp = %+v, want key %s", resp, tc.want)
		}
		if tc.req.ContentType != "" && resp.Headers["Content-Type"] != tc.req.ContentType {
			t.Errorf("headers = %v", resp.Headers)
		}
		if resp.Headers == nil {
			t.Errorf("headers is nil")
		}
		key, ok := e.svc.parseUploadID(resp.UploadID)
		if !ok || key != tc.want {
			t.Errorf("upload ID gives %q %v", key, ok)
		}
	}
}

func TestUploadRejects(t *testing.T) {
	ff := &fakeFiles{}
	e := newEnv(t, func(c *Config) {
		c.Files = ff
		c.MaxUploadBytes = map[runnerproto.UploadKind]int64{runnerproto.UploadArtifact: 10}
	})
	ok := runnerproto.UploadRequest{Kind: "artifact", Name: "a", Bytes: 5, SHA256: sha}
	mod := func(f func(*runnerproto.UploadRequest)) runnerproto.UploadRequest { r := ok; f(&r); return r }
	cases := map[string]struct {
		req  runnerproto.UploadRequest
		code int
	}{
		"too large":       {mod(func(r *runnerproto.UploadRequest) { r.Bytes = 11 }), 413},
		"zero bytes":      {mod(func(r *runnerproto.UploadRequest) { r.Bytes = 0 }), 400},
		"dot dot":         {mod(func(r *runnerproto.UploadRequest) { r.Name = "a/../b" }), 400},
		"absolute":        {mod(func(r *runnerproto.UploadRequest) { r.Name = "/a" }), 400},
		"bad sha":         {mod(func(r *runnerproto.UploadRequest) { r.SHA256 = "ABC" }), 400},
		"bad kind":        {mod(func(r *runnerproto.UploadRequest) { r.Kind = "other" }), 400},
		"snapshot part":   {runnerproto.UploadRequest{Kind: "snapshot", Part: "x", Name: "a", Bytes: 5, SHA256: sha}, 400},
		"snapshot nopart": {runnerproto.UploadRequest{Kind: "snapshot", Name: "a", Bytes: 5, SHA256: sha}, 400},
		"artifact part":   {mod(func(r *runnerproto.UploadRequest) { r.Part = "workspace" }), 400},
		"backslash name":  {mod(func(r *runnerproto.UploadRequest) { r.Name = "a\\b" }), 400},
	}
	for name, tc := range cases {
		if code, body := e.call("POST", runnerproto.PathSessionUploads, 1, tc.req); code != tc.code {
			t.Errorf("%s: code %d, want %d (%s)", name, code, tc.code, body)
		}
	}
	if len(ff.puts) != 0 {
		t.Errorf("presigned %v", ff.puts)
	}
	// The default limit of a snapshot is large.
	big := runnerproto.UploadRequest{Kind: "snapshot", Part: "workspace", Name: "a", Bytes: 1 << 30, SHA256: sha}
	if code, body := e.call("POST", runnerproto.PathSessionUploads, 1, big); code != 200 {
		t.Errorf("1 GiB snapshot: %d %s", code, body)
	}
}

func TestTransferNotImplementedWithoutFiles(t *testing.T) {
	e := newEnv(t, nil)
	up := runnerproto.UploadRequest{Kind: "artifact", Name: "a", Bytes: 5, SHA256: sha}
	if code, _ := e.call("POST", runnerproto.PathSessionUploads, 1, up); code != 501 {
		t.Errorf("uploads: %d", code)
	}
	if code, _ := e.call("GET", runnerproto.PathSessionDownloads+"?kind=snapshot", 1, nil); code != 501 {
		t.Errorf("downloads: %d", code)
	}
	if code, _ := e.call("POST", runnerproto.PathGitCredential, 1, runnerproto.GitCredentialRequest{Protocol: "https", Host: "h", Path: "p"}); code != 501 {
		t.Errorf("git credential: %d", code)
	}
}

func TestUploadIDForged(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Files = &fakeFiles{} })
	key := "runs/" + e.run.ID + "/sessions/1/artifact/a"
	id := e.svc.uploadID(key)
	if k, ok := e.svc.parseUploadID(id); !ok || k != key {
		t.Fatalf("round trip: %q %v", k, ok)
	}
	other := e.svc.uploadID("runs/other/sessions/1/artifact/a")
	forged := id[:strings.Index(id, ".")] + other[strings.Index(other, "."):]
	for _, bad := range []string{"", "up_", "up_x", "up_x.y", forged, strings.TrimPrefix(id, "up_"), id + "A"} {
		if _, ok := e.svc.parseUploadID(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if !ownsKey(e.run, key) || !ownsKey(e.run, "snapshots/team_1/"+e.run.ID+"/1/workspace/a") {
		t.Error("ownsKey refuses own keys")
	}
	for _, k := range []string{"runs/other/x", "snapshots/team_2/" + e.run.ID + "/1/workspace/a", "snapshots/team_1/other/1/workspace/a"} {
		if ownsKey(e.run, k) {
			t.Errorf("ownsKey accepts %q", k)
		}
	}
}

// saveSnapshot stores a snapshot.saved event for a session and returns the
// keys. All files are marked as existing in ff.
func saveSnapshot(t *testing.T, e *env, ff *fakeFiles, session int, part string, seq int64, names ...string) {
	t.Helper()
	var ids []string
	for _, n := range names {
		key := fmt.Sprintf("snapshots/team_1/%s/%d/%s/%s", e.run.ID, session, part, n)
		ids = append(ids, e.svc.uploadID(key))
		if ff.objects == nil {
			ff.objects = map[string]int64{}
		}
		ff.objects[key] = int64(len(n))
	}
	data, _ := json.Marshal(runnerproto.SnapshotSavedData{Part: part, UploadIDs: ids, Bytes: 1})
	ev := core.RunnerEvent{Seq: seq, Type: string(runnerproto.EventSnapshotSaved), At: time.Now(), Data: data}
	if _, err := e.mem.AppendRunnerEvents(context.Background(), e.run.ID, session, []core.RunnerEvent{ev}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) endSession(n int) {
	e.t.Helper()
	s := e.session(n)
	s.State = core.SessionEnded
	if err := e.mem.UpdateRunSession(context.Background(), s); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) downloads(session int) []runnerproto.DownloadFile {
	e.t.Helper()
	code, body := e.call("GET", runnerproto.PathSessionDownloads+"?kind=snapshot", session, nil)
	if code != 200 {
		e.t.Fatalf("downloads: %d %s", code, body)
	}
	var resp runnerproto.DownloadsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		e.t.Fatal(err)
	}
	if resp.Files == nil {
		e.t.Fatalf("files is null: %s", body)
	}
	return resp.Files
}

func names(files []runnerproto.DownloadFile) []string {
	var out []string
	for _, f := range files {
		out = append(out, f.Part+":"+f.Name)
	}
	return out
}

func TestDownloadsNoSnapshot(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Files = &fakeFiles{} })
	if files := e.downloads(1); len(files) != 0 {
		t.Errorf("files = %v", files)
	}
}

func TestDownloadsNewestSnapshot(t *testing.T) {
	ff := &fakeFiles{}
	e := newEnv(t, func(c *Config) { c.Files = ff })
	// Session 1 saves both parts. Session 2 saves a newer workspace only.
	saveSnapshot(t, e, ff, 1, "workspace", 1, "old.go")
	saveSnapshot(t, e, ff, 1, "harness_state", 2, "state/s.json")
	e.endSession(1)
	e.newSession(2, time.Hour)
	// Session 2 only sees session 1.
	if got := names(e.downloads(2)); strings.Join(got, ",") != "workspace:old.go,harness_state:state/s.json" {
		t.Errorf("session 2 sees %v", got)
	}
	saveSnapshot(t, e, ff, 2, "workspace", 1, "new.go", "dir/b.go")
	e.endSession(2)
	e.newSession(3, time.Hour)

	got := names(e.downloads(3))
	want := []string{"workspace:new.go", "workspace:dir/b.go", "harness_state:state/s.json"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}
	files := e.downloads(3)
	if files[0].Bytes != int64(len("new.go")) || files[0].ExpiresAt.IsZero() ||
		files[0].URL != "https://files.test/get/snapshots/team_1/"+e.run.ID+"/2/workspace/new.go" {
		t.Errorf("file = %+v", files[0])
	}
}

func TestDownloadsSkipsBadEntries(t *testing.T) {
	ff := &fakeFiles{}
	e := newEnv(t, func(c *Config) { c.Files = ff })
	saveSnapshot(t, e, ff, 1, "workspace", 1, "a.go", "gone.go")
	delete(ff.objects, fmt.Sprintf("snapshots/team_1/%s/1/workspace/gone.go", e.run.ID))
	// A forged ID in a later event of the same part is a different event; add one in part harness_state.
	data, _ := json.Marshal(runnerproto.SnapshotSavedData{Part: "harness_state", UploadIDs: []string{"up_bad.bad"}})
	ev := core.RunnerEvent{Seq: 2, Type: string(runnerproto.EventSnapshotSaved), At: time.Now(), Data: data}
	if _, err := e.mem.AppendRunnerEvents(context.Background(), e.run.ID, 1, []core.RunnerEvent{ev}); err != nil {
		t.Fatal(err)
	}
	e.endSession(1)
	e.newSession(2, time.Hour)
	if got := names(e.downloads(2)); strings.Join(got, ",") != "workspace:a.go" {
		t.Errorf("files = %v", got)
	}
}

func TestDownloadsKinds(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Files = &fakeFiles{} })
	if code, _ := e.call("GET", runnerproto.PathSessionDownloads+"?kind=attachment", 1, nil); code != 501 {
		t.Errorf("attachment: %d", code)
	}
	if code, _ := e.call("GET", runnerproto.PathSessionDownloads+"?kind=other", 1, nil); code != 400 {
		t.Errorf("other: %d", code)
	}
	if code, _ := e.call("GET", runnerproto.PathSessionDownloads, 1, nil); code != 400 {
		t.Errorf("none: %d", code)
	}
}

type fakeGit struct {
	resp runnerproto.GitCredentialResponse
	err  error
	got  runnerproto.GitCredentialRequest
}

func (f *fakeGit) GitCredential(_ context.Context, _ *core.Run, _ *core.RunSession, req runnerproto.GitCredentialRequest) (runnerproto.GitCredentialResponse, error) {
	f.got = req
	return f.resp, f.err
}

func TestGitCredential(t *testing.T) {
	fg := &fakeGit{resp: runnerproto.GitCredentialResponse{Username: "u", Password: "p", ExpiresAt: time.Now().Add(time.Hour)}}
	e := newEnv(t, func(c *Config) { c.GitCredentials = fg })
	call := func(req runnerproto.GitCredentialRequest) (int, []byte) {
		return e.call("POST", runnerproto.PathGitCredential, 1, req)
	}
	good := runnerproto.GitCredentialRequest{Protocol: "https", Host: "github.com", Path: "acme/api"}
	if code, _ := call(runnerproto.GitCredentialRequest{Protocol: "ssh", Host: "h", Path: "p"}); code != 400 {
		t.Errorf("protocol: %d", code)
	}
	if code, _ := call(runnerproto.GitCredentialRequest{Protocol: "https", Path: "p"}); code != 400 {
		t.Errorf("host: %d", code)
	}
	if code, _ := call(runnerproto.GitCredentialRequest{Protocol: "https", Host: "h"}); code != 400 {
		t.Errorf("path: %d", code)
	}
	fg.err = fmt.Errorf("no: %w", core.ErrForbidden)
	if code, body := call(good); code != 403 || !strings.Contains(string(body), `"forbidden"`) {
		t.Errorf("forbidden: %d %s", code, body)
	}
	fg.err = errors.New("boom")
	if code, body := call(good); code != 500 || strings.Contains(string(body), "boom") {
		t.Errorf("error: %d %s", code, body)
	}
	fg.err = nil
	code, body := call(good)
	var resp runnerproto.GitCredentialResponse
	if code != 200 || json.Unmarshal(body, &resp) != nil || resp.Username != "u" || resp.Password != "p" {
		t.Errorf("ok: %d %s", code, body)
	}
	if fg.got != good {
		t.Errorf("provider got %+v", fg.got)
	}
}

// A snapshot upload uses SnapshotFiles. An artifact upload keeps using Files.
func TestSnapshotUsesOwnStore(t *testing.T) {
	arts, snaps := &fakeFiles{}, &fakeFiles{}
	e := newEnv(t, func(c *Config) {
		c.Files = arts
		c.SnapshotFiles = snaps
		c.SnapshotPutHeaders = func(string) map[string]string { return map[string]string{"X-Snap": "1"} }
	})
	snap := runnerproto.UploadRequest{Kind: "snapshot", Part: "workspace", Name: "a", Bytes: 5, SHA256: sha}
	code, body := e.call("POST", runnerproto.PathSessionUploads, 1, snap)
	if code != 200 || !strings.Contains(string(body), "X-Snap") {
		t.Fatalf("snapshot upload: %d %s", code, body)
	}
	art := runnerproto.UploadRequest{Kind: "artifact", Name: "a", Bytes: 5, SHA256: sha}
	if code, body := e.call("POST", runnerproto.PathSessionUploads, 1, art); code != 200 {
		t.Fatalf("artifact upload: %d %s", code, body)
	}
	if len(snaps.puts) != 1 || !strings.HasPrefix(snaps.puts[0], "snapshots/") || len(arts.puts) != 1 || strings.HasPrefix(arts.puts[0], "snapshots/") {
		t.Fatalf("snapshot puts %v, artifact puts %v", snaps.puts, arts.puts)
	}
}

func TestSnapshotDownloadsUseOwnStore(t *testing.T) {
	arts, snaps := &fakeFiles{}, &fakeFiles{}
	e := newEnv(t, func(c *Config) { c.Files = arts; c.SnapshotFiles = snaps })
	saveSnapshot(t, e, snaps, 1, "workspace", 1, "a.go")
	e.endSession(1)
	e.newSession(2, time.Hour)
	if got := names(e.downloads(2)); strings.Join(got, ",") != "workspace:a.go" {
		t.Errorf("files = %v", got)
	}
	if len(snaps.gets) != 1 || len(arts.gets) != 0 {
		t.Errorf("snapshot gets %v, artifact gets %v", snaps.gets, arts.gets)
	}
}

func TestSnapshotMaxBytes(t *testing.T) {
	ff := &fakeFiles{}
	e := newEnv(t, func(c *Config) { c.Files = ff; c.SnapshotMaxBytes = 100 })
	mk := func(kind, part string, n int64) runnerproto.UploadRequest {
		return runnerproto.UploadRequest{Kind: runnerproto.UploadKind(kind), Part: part, Name: "a", Bytes: n, SHA256: sha}
	}
	if code, body := e.call("POST", runnerproto.PathSessionUploads, 1, mk("snapshot", "workspace", 101)); code != 413 || !strings.Contains(string(body), "too_large") {
		t.Errorf("101 bytes: %d %s", code, body)
	}
	if code, body := e.call("POST", runnerproto.PathSessionUploads, 1, mk("snapshot", "workspace", 100)); code != 200 {
		t.Errorf("100 bytes: %d %s", code, body)
	}
	// The limit does not apply to artifacts.
	if code, body := e.call("POST", runnerproto.PathSessionUploads, 1, mk("artifact", "", 101)); code != 200 {
		t.Errorf("artifact 101 bytes: %d %s", code, body)
	}
	if len(ff.puts) != 2 {
		t.Errorf("puts %v", ff.puts)
	}
}
