package server

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/objectstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// Retention deletes uploaded attachments (the files and their metadata) that
// are older than the retention period, and keeps the newer ones.
func TestPruneRunDataDeletesOldAttachments(t *testing.T) {
	ctx := context.Background()
	fs, err := objectstore.Open(ctx, "file://"+t.TempDir(), objectstore.Options{SigningKey: []byte("k")})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := objectstore.NewRunData(fs, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := testdb.New(t)
	s := &Server{RunData: rd, Store: mem, retention: 100 * time.Millisecond, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	upload := func(id string) string {
		key := "attachments/team_1/" + id + "/a.txt"
		if err := fs.Put(ctx, key, strings.NewReader("x"), 1, "text/plain"); err != nil {
			t.Fatal(err)
		}
		meta, _ := core.MarshalAttachment(core.Attachment{ID: id, TeamID: "team_1", Name: "a.txt", Key: key})
		if err := mem.KVSet(ctx, core.AttachmentKVKey(id), meta); err != nil {
			t.Fatal(err)
		}
		return key
	}
	oldKey := upload("att_old")
	time.Sleep(250 * time.Millisecond)
	newKey := upload("att_new")

	s.pruneRunData(ctx)

	if _, err := fs.Stat(ctx, oldKey); err == nil {
		t.Error("the old attachment file survived")
	}
	if _, err := mem.KVGet(ctx, core.AttachmentKVKey("att_old")); err == nil {
		t.Error("the old attachment metadata survived")
	}
	if _, err := fs.Stat(ctx, newKey); err != nil {
		t.Errorf("the new attachment file is gone: %v", err)
	}
	if _, err := mem.KVGet(ctx, core.AttachmentKVKey("att_new")); err != nil {
		t.Errorf("the new attachment metadata is gone: %v", err)
	}
}
