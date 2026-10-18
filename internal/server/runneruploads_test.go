package server

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/objectstore"
)

func TestUploadStorer(t *testing.T) {
	ctx := context.Background()
	fs, err := objectstore.NewFileStore(t.TempDir(), objectstore.Options{SigningKey: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := objectstore.NewRunData(fs, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	put := func(key, body string) {
		t.Helper()
		if err := fs.Put(ctx, key, strings.NewReader(body), int64(len(body)), "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	art := "runs/run1/sessions/2/artifact/out/report.txt"
	tr := "runs/run1/sessions/2/transcript/transcript.jsonl"
	put(art, "report body")
	put(tr, `{"line":1}`+"\n")
	ids := map[string]string{"up_art": art, "up_tr": tr, "up_other": "runs/run2/sessions/2/artifact/x"}
	u := &uploadStorer{runData: rd, keyOf: func(id string) (string, bool) { k, ok := ids[id]; return k, ok }}

	if err := u.store(ctx, "run1", 2, "artifact", "out/report.txt", "up_art"); err != nil {
		t.Fatal(err)
	}
	list, err := rd.ListArtifacts(ctx, "run1")
	if err != nil || len(list) != 1 || list[0].Key != "out/report.txt" {
		t.Fatalf("artifact list = %+v, %v", list, err)
	}
	rc, _, err := rd.OpenArtifact(ctx, "run1", "out/report.txt")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(rc); string(b) != "report body" {
		t.Errorf("artifact body = %q", b)
	}
	rc.Close()

	if err := u.store(ctx, "run1", 2, "transcript", "", "up_tr"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.Get(ctx, "runs/run1/harness/2.jsonl"); err != nil {
		t.Errorf("harness transcript: %v", err)
	}

	// Another run's upload, an unknown ID, a wrong session and a wrong kind all fail.
	for name, call := range map[string]func() error{
		"other run":     func() error { return u.store(ctx, "run1", 2, "artifact", "x", "up_other") },
		"unknown id":    func() error { return u.store(ctx, "run1", 2, "artifact", "x", "up_zzz") },
		"other session": func() error { return u.store(ctx, "run1", 3, "artifact", "x", "up_art") },
		"other kind":    func() error { return u.store(ctx, "run1", 2, "transcript", "", "up_art") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
