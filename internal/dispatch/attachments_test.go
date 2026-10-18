package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestToItemAttachmentLookup(t *testing.T) {
	m := core.RunInput{ID: "msg_1", RunID: "run_1", Attachments: []string{"att_1", "att_gone"}}
	lookup := func(_ context.Context, id string) (core.Attachment, error) {
		if id == "att_gone" {
			return core.Attachment{}, errors.New("missing")
		}
		return core.Attachment{ID: id, Name: "a.txt", ContentType: "text/plain", Bytes: 7}, nil
	}
	item := toItem(context.Background(), lookup, m)
	if len(item.Attachments) != 2 {
		t.Fatalf("attachments = %+v", item.Attachments)
	}
	a := item.Attachments[0]
	if a.ID != "att_1" || a.Name != "a.txt" || a.ContentType != "text/plain" || a.Bytes != 7 || a.Path != "" {
		t.Fatalf("first = %+v", a)
	}
	if b := item.Attachments[1]; b.ID != "att_gone" || b.Name != "" || b.Bytes != 0 {
		t.Fatalf("failed lookup = %+v", b)
	}
	// Without a lookup only the IDs are set.
	if got := toItem(context.Background(), nil, m).Attachments[0]; got.Name != "" || got.ID != "att_1" {
		t.Fatalf("no lookup = %+v", got)
	}
}
