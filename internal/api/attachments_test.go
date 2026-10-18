package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/objectstore"
)

func attachmentEnv(t *testing.T) *testEnv {
	t.Helper()
	fs, err := objectstore.NewFileStore(t.TempDir(), objectstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rd, err := objectstore.NewRunData(fs, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return newTestEnv(t, func(o *Options) { o.RunData = rd })
}

func (e *testEnv) upload(query, key string, body []byte) resp {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+BasePath+"/attachments"+query, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "text/plain")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func TestUploadAttachment(t *testing.T) {
	e := attachmentEnv(t)
	r := e.upload("?name="+"..%5Cdir%2Fnotes.txt", e.viewer.Key, []byte("hello"))
	if r.Status != 201 {
		t.Fatalf("status %d: %s", r.Status, r.Raw)
	}
	if r.Body["name"] != "notes.txt" || r.Body["content_type"] != "text/plain" || r.Body["bytes"].(float64) != 5 {
		t.Fatalf("body = %s", r.Raw)
	}
	id := str(r.Body, "attachment_id")
	if !strings.HasPrefix(id, "att_") {
		t.Fatal(id)
	}
	if _, leaked := r.Body["Key"]; leaked || strings.Contains(string(r.Raw), "attachments/") {
		t.Fatalf("object key leaked: %s", r.Raw)
	}
	raw, err := e.store.KVGet(context.Background(), core.AttachmentKVKey(id))
	if err != nil {
		t.Fatal(err)
	}
	att, err := core.UnmarshalAttachment(raw)
	if err != nil || att.Key != "attachments/"+att.TeamID+"/"+id+"/notes.txt" {
		t.Fatalf("stored %+v err %v", att, err)
	}

	if r := e.upload("", e.viewer.Key, []byte("x")); r.Status != 400 {
		t.Fatalf("missing name: %d %s", r.Status, r.Raw)
	}
	if r := e.upload("?name=..", e.viewer.Key, []byte("x")); r.Status != 400 {
		t.Fatalf("dot name: %d", r.Status)
	}
	big := make([]byte, core.MaxAttachmentBytes+1)
	if r := e.upload("?name=big.bin", e.viewer.Key, big); r.Status != 413 || errCode(r) != CodePayloadTooLarge {
		t.Fatalf("too large: %d %s", r.Status, r.Raw)
	}
	if r := e.upload("?name=a", "", []byte("x")); r.Status != 401 {
		t.Fatalf("no key: %d", r.Status)
	}
}

func TestMessageAttachments(t *testing.T) {
	e := attachmentEnv(t)
	run := e.must(e.do("POST", "/runs", e.viewer.Key, runBody(e.createFactory("files"), "hi")), 201)
	runID := str(run.Body, "run_id")
	path := "/runs/" + runID + "/followups"

	e.must(e.do("POST", path, e.viewer.Key, map[string]any{"message": "see file", "attachments": []string{"att_missing"}}), 400)

	own := e.upload("?name=a.txt", e.viewer.Key, []byte("a"))
	id := str(own.Body, "attachment_id")
	other := e.upload("?name=b.txt", e.other.Key, []byte("b"))
	if r := e.do("POST", path, e.viewer.Key, map[string]any{"message": "x", "attachments": []string{str(other.Body, "attachment_id")}}); r.Status != 400 {
		t.Fatalf("other team's attachment: %d %s", r.Status, r.Raw)
	}
	tooMany := make([]string, core.MaxAttachmentsPerMessage+1)
	for i := range tooMany {
		tooMany[i] = id
	}
	e.must(e.do("POST", path, e.viewer.Key, map[string]any{"message": "x", "attachments": tooMany}), 400)

	e.must(e.do("POST", path, e.viewer.Key, map[string]any{"message": "see file", "attachments": []string{id}}), 202)
	calls := e.engine.CallsTo("FollowUp")
	last := calls[len(calls)-1]
	if got, _ := last.Args[len(last.Args)-1].([]string); len(got) != 1 || got[0] != id {
		t.Fatalf("engine call = %#v", last)
	}
}
