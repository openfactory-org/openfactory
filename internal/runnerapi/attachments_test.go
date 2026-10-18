package runnerapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func TestAttachmentDownload(t *testing.T) {
	ff := &fakeFiles{objects: map[string]int64{"attachments/t/att_1/a.txt": 9, "attachments/x/att_3/c.txt": 3}}
	var e *env
	atts := map[string]core.Attachment{}
	e = newEnv(t, func(c *Config) {
		c.Files = ff
		c.Attachments = func(_ context.Context, id string) (core.Attachment, error) {
			a, ok := atts[id]
			if !ok {
				return core.Attachment{}, core.ErrNotFound
			}
			return a, nil
		}
	})
	team := teamOf(e.run)
	atts["att_1"] = core.Attachment{ID: "att_1", TeamID: team, Name: "a.txt", Bytes: 9, Key: "attachments/t/att_1/a.txt"}
	atts["att_2"] = core.Attachment{ID: "att_2", TeamID: team, Name: "b.txt", Key: "attachments/t/att_2/b.txt"}
	atts["att_3"] = core.Attachment{ID: "att_3", TeamID: "other-team", Name: "c.txt", Key: "attachments/x/att_3/c.txt"}
	// att_1 and att_3 are in inputs of the run. att_2 is not.
	_, err := e.mem.AppendRunInput(context.Background(), &core.RunInput{
		ID: "msg_1", RunID: e.run.ID, Kind: "user", Content: "x", Attachments: []string{"att_1", "att_3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string) (int, []byte) {
		return e.call("GET", runnerproto.PathSessionDownloads+"?kind=attachment&id="+id, 1, nil)
	}
	code, body := get("att_1")
	if code != 200 {
		t.Fatalf("ok: %d %s", code, body)
	}
	var resp runnerproto.DownloadsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 || resp.Files[0].Part != "attachment" || resp.Files[0].Name != "a.txt" ||
		resp.Files[0].Bytes != 9 || resp.Files[0].URL != "https://files.test/get/attachments/t/att_1/a.txt" {
		t.Fatalf("files = %+v", resp.Files)
	}
	if code, _ := get("att_2"); code != 404 {
		t.Errorf("not in the inputs of the run: %d", code)
	}
	if code, _ := get("att_3"); code != 404 {
		t.Errorf("other team: %d", code)
	}
	if code, _ := get("att_nope"); code != 404 {
		t.Errorf("unknown: %d", code)
	}
	if code, _ := e.call("GET", runnerproto.PathSessionDownloads+"?kind=attachment", 1, nil); code != 400 {
		t.Errorf("no id: %d", code)
	}
}
