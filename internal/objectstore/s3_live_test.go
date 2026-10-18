package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveS3 opens a store on a real S3-compatible server. The test is skipped
// unless OF_TEST_S3_URL is set, for example
//
//	OF_TEST_S3_URL='s3://bucket?region=us-east-1&endpoint=http://127.0.0.1:8333&path_style=true'
//
// The bucket must exist. The credentials come from AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY (or a role, see awscreds.go); AWS_CA_BUNDLE adds a
// private CA. For the local AWS stack (deploy/aws-local), source its
// server.env and set OF_TEST_S3_URL="$OF_OBJECT_STORE_URL" and
// OF_TEST_S3_MAX_OBJECTS=1000. Every test writes below its own random prefix and
// deletes its objects at the end.
func liveS3(t *testing.T) Store {
	t.Helper()
	raw := os.Getenv("OF_TEST_S3_URL")
	if raw == "" {
		t.Skip("OF_TEST_S3_URL is not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = fmt.Sprintf("/livetest-%d", time.Now().UnixNano())
	s, err := Open(context.Background(), u.String(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		objs, _ := s.List(ctx, "")
		for _, o := range objs {
			_ = s.Delete(ctx, o.Key)
		}
	})
	return s
}

func TestLiveS3Store(t *testing.T) {
	s := liveS3(t)
	ctx := context.Background()
	keys := []string{
		"runs/run_1/artifacts/plain.txt",
		"runs/run_1/artifacts/data/sub dir/café+plus=eq&amp.txt",
		"runs/run_1/artifacts/my file (1).txt",
		"runs/run_1/artifacts/tilde~and_under-score.bin",
	}
	for _, k := range keys {
		body := "body of " + k
		if err := s.Put(ctx, k, strings.NewReader(body), int64(len(body)), "text/plain"); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
		rc, info, err := s.Get(ctx, k)
		if err != nil {
			t.Fatalf("get %q: %v", k, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != body || info.Size != int64(len(body)) {
			t.Fatalf("get %q = %q (size %d)", k, got, info.Size)
		}
		st, err := s.Stat(ctx, k)
		if err != nil || st.Size != int64(len(body)) || st.ContentType != "text/plain" {
			t.Fatalf("stat %q = %+v, %v", k, st, err)
		}
	}
	// An empty object.
	if err := s.Put(ctx, "runs/run_1/artifacts/empty", strings.NewReader(""), 0, ""); err != nil {
		t.Fatalf("put empty: %v", err)
	}
	// Unknown size is buffered.
	if err := s.Put(ctx, "runs/run_1/artifacts/unknown-size", strings.NewReader("abc"), -1, ""); err != nil {
		t.Fatalf("put unknown size: %v", err)
	}
	if _, _, err := s.Get(ctx, "runs/run_1/artifacts/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(ctx, "runs/run_1/artifacts/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat missing = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "runs/run_1/artifacts/missing"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	objs, err := s.List(ctx, "runs/run_1/artifacts/")
	if err != nil || len(objs) != len(keys)+2 {
		t.Fatalf("list = %d objects, %v; want %d", len(objs), err, len(keys)+2)
	}
	for i := 1; i < len(objs); i++ {
		if objs[i-1].Key >= objs[i].Key {
			t.Fatalf("list not sorted: %q before %q", objs[i-1].Key, objs[i].Key)
		}
	}
	if objs, _ := s.List(ctx, "runs/run_2/"); len(objs) != 0 {
		t.Fatalf("list of another run = %v", objs)
	}

	// Presigned GET and PUT with plain HTTP.
	get, err := s.PresignGet(ctx, keys[1], time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if body := httpDo(t, http.MethodGet, get, nil, nil, http.StatusOK); body != "body of "+keys[1] {
		t.Fatalf("presigned get body = %q", body)
	}
	put, err := s.PresignPut(ctx, "runs/run_1/attachments/up load.txt", time.Minute, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	httpDo(t, http.MethodPut, put, strings.NewReader("uploaded"), PutHeaders(s, "text/plain"), http.StatusOK)
	httpDo(t, http.MethodPut, put, strings.NewReader("uploaded"), nil, http.StatusForbidden) // content type is signed
	if st, err := s.Stat(ctx, "runs/run_1/attachments/up load.txt"); err != nil || st.Size != 8 {
		t.Fatalf("stat after presigned put = %+v, %v", st, err)
	}
	// A tampered signature and an expired link are refused.
	httpDo(t, http.MethodGet, strings.Replace(get, "X-Amz-Signature=", "X-Amz-Signature=00", 1), nil, nil, http.StatusForbidden)
	short, _ := s.PresignGet(ctx, keys[0], time.Second)
	time.Sleep(2500 * time.Millisecond)
	httpDo(t, http.MethodGet, short, nil, nil, http.StatusForbidden)

	if err := s.Delete(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, keys[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stat after delete = %v", err)
	}
}

// TestLiveS3ListPagination checks ListObjectsV2 continuation (more than 1000
// objects). Set OF_TEST_S3_MAX_OBJECTS for a store that keeps fewer objects
// per bucket (the local AWS emulator keeps 1000); the test is then skipped,
// because a list of at most 1000 keys has no second page.
func TestLiveS3ListPagination(t *testing.T) {
	const n = 1105
	if v := os.Getenv("OF_TEST_S3_MAX_OBJECTS"); v != "" {
		if max, err := strconv.Atoi(v); err == nil && max < n {
			t.Skipf("OF_TEST_S3_MAX_OBJECTS=%d: the store cannot hold the %d objects that a second ListObjectsV2 page needs", max, n)
		}
	}
	s := liveS3(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	sem := make(chan struct{}, 16)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs <- s.Put(ctx, fmt.Sprintf("runs/run_p/artifacts/f%05d", i), bytes.NewReader([]byte("x")), 1, "")
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	objs, err := s.List(ctx, "runs/run_p/")
	if err != nil || len(objs) != n {
		t.Fatalf("list = %d objects, %v; want %d", len(objs), err, n)
	}
}

func TestLiveS3RunData(t *testing.T) {
	s := liveS3(t)
	ctx := context.Background()
	rd, err := NewRunData(s, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.PutArtifact(ctx, "run_a", "out/result file.json", strings.NewReader(`{"ok":true}`), 11, "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.PutAttachment(ctx, "run_a", "in.txt", strings.NewReader("in"), 2, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.PutHarnessTranscript(ctx, "run_a", 1, strings.NewReader("{\"a\":1}\n"), 8); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "snapshots/team_1/run_a/1/workspace/manifest.json", strings.NewReader("{}"), 2, ""); err != nil {
		t.Fatal(err)
	}
	arts, err := rd.ListArtifacts(ctx, "run_a")
	if err != nil || len(arts) != 1 || arts[0].Key != "out/result file.json" {
		t.Fatalf("artifacts = %+v, %v", arts, err)
	}
	u, err := rd.ArtifactURL(ctx, "run_a", "out/result file.json", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if body := httpDo(t, http.MethodGet, u, nil, nil, http.StatusOK); body != `{"ok":true}` {
		t.Fatalf("artifact body = %q", body)
	}

	// Retention: nothing is old yet, then everything is.
	if pruned, err := rd.Prune(ctx, time.Hour); err != nil || len(pruned) != 0 {
		t.Fatalf("prune (fresh) = %v, %v", pruned, err)
	}
	rd.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	pruned, err := rd.Prune(ctx, time.Hour)
	if err != nil || len(pruned) != 1 || pruned[0] != "run_a" {
		t.Fatalf("prune (old) = %v, %v", pruned, err)
	}
	if objs, _ := s.List(ctx, ""); len(objs) != 0 {
		t.Fatalf("objects left after prune: %v", objs)
	}
}

func httpDo(t *testing.T, method, u string, body io.Reader, hdr http.Header, want int) string {
	t.Helper()
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	// A presigned URL is used as a browser or a task Pod would: with the
	// trusted CAs, plus AWS_CA_BUNDLE for a store behind a private CA.
	client, err := s3HTTPClient(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, strings.SplitN(u, "?", 2)[0], resp.StatusCode, want, b)
	}
	return string(b)
}
