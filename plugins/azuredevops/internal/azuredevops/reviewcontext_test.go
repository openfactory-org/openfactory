package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
)

func TestUnifiedDiff(t *testing.T) {
	if unifiedDiff("a\nb\n", "a\nb\n") != "" {
		t.Fatal("same text gives a diff")
	}
	got := unifiedDiff("one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n", "one\ntwo\nthree\nfour\nFIVE\nsix\nseven\neight\nnine\nten\neleven\n")
	// The first change has 3 lines of context on each side. The second
	// change (one added line at the end) is a hunk of its own only when far
	// enough: here the two changes are 5 lines apart, so they join.
	if !strings.HasPrefix(got, "@@ -2,9 +2,10 @@\n two\n three\n four\n-five\n+FIVE\n six\n seven\n eight\n nine\n ten\n+eleven\n") {
		t.Fatalf("diff:\n%s", got)
	}
	// A new file and a deleted file.
	if got := unifiedDiff("", "a\nb\n"); got != "@@ -0,0 +1,2 @@\n+a\n+b\n" {
		t.Fatalf("new file:\n%s", got)
	}
	if got := unifiedDiff("a\nb\n", ""); got != "@@ -1,2 +0,0 @@\n-a\n-b\n" {
		t.Fatalf("deleted file:\n%s", got)
	}
	// Two changes far apart make two hunks.
	var a, b []string
	for i := 0; i < 30; i++ {
		a = append(a, fmt.Sprint("l", i))
		b = append(b, fmt.Sprint("l", i))
	}
	b[2], b[25] = "X", "Y"
	got = unifiedDiff(strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n")
	if strings.Count(got, "@@ -") != 2 || !strings.Contains(got, "@@ -23,7 +23,7 @@") {
		t.Fatalf("two hunks:\n%s", got)
	}
	// Too many lines for the table: the middle is one replaced block.
	big := strings.Repeat("x\n", 3000)
	if got := unifiedDiff(big+"a\n", big+"b\n"); !strings.Contains(got, "-a\n+b\n") {
		t.Fatalf("large diff:\n%s", got[:min(len(got), 200)])
	}
}

func TestReviewThreadAndFileDiff(t *testing.T) {
	const base = "/contoso/proj/_apis/git/repositories/web"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case base + "/pullRequests/11/threads/66":
			fmt.Fprint(w, `{"id":66,"status":"fixed","threadContext":{"filePath":"/src/app.go","rightFileStart":{"line":4,"offset":1}},
				"comments":[
				{"id":1,"content":"rename it","commentType":"text","publishedDate":"2026-01-02T03:04:05Z","author":{"displayName":"Dana","uniqueName":"dana@contoso.com"}},
				{"id":2,"content":"system line","commentType":"system","author":{"displayName":"x"}},
				{"id":3,"content":"gone","commentType":"text","isDeleted":true,"author":{"displayName":"y"}},
				{"id":4,"content":"done","commentType":"text","author":{"displayName":"Bot"}}]}`)
		case base + "/pullRequests/11/iterations":
			fmt.Fprint(w, `{"value":[{"id":1,"sourceRefCommit":{"commitId":"s1"},"commonRefCommit":{"commitId":"m1"}},
				{"id":2,"sourceRefCommit":{"commitId":"s2"},"commonRefCommit":{"commitId":"m2"}}]}`)
		case base + "/pullRequests/11/iterations/2/changes":
			if r.URL.Query().Get("$compareTo") != "0" {
				t.Errorf("changes query = %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"changeEntries":[
				{"changeType":"edit","item":{"path":"/src/app.go"}},
				{"changeType":"add","item":{"path":"/src/new.go"}},
				{"changeType":"edit, rename","originalPath":"/src/old.go","item":{"path":"/src/moved.go"}},
				{"changeType":"edit","item":{"path":"/img.bin"}}]}`)
		case base + "/items":
			q := r.URL.Query()
			if q.Get("$format") != "octetstream" || q.Get("versionDescriptor.versionType") != "commit" {
				t.Errorf("items query = %s", r.URL.RawQuery)
			}
			switch q.Get("path") + "@" + q.Get("versionDescriptor.version") {
			case "/src/app.go@m2":
				fmt.Fprint(w, "package app\n\nfunc a() {}\n")
			case "/src/app.go@s2":
				fmt.Fprint(w, "package app\n\nfunc b() {}\n")
			case "/src/new.go@s2":
				fmt.Fprint(w, "package app\n")
			case "/src/old.go@m2":
				fmt.Fprint(w, "x\n")
			case "/src/moved.go@s2":
				fmt.Fprint(w, "y\n")
			case "/img.bin@m2", "/img.bin@s2":
				w.Write([]byte{0x89, 'P', 0, 'G'})
			default:
				t.Errorf("unexpected item %s", r.URL.RawQuery)
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Config{Organization: "contoso", BaseURL: srv.URL, Token: "pat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	th, err := c.ReviewThread(ctx, "proj", "web", 11, "66")
	if err != nil {
		t.Fatal(err)
	}
	if th.Path != "src/app.go" || th.Line != 4 || !th.Resolved || len(th.Comments) != 2 || th.Comments[0].Author != "dana@contoso.com" || th.Comments[1].Author != "Bot" {
		t.Fatalf("thread = %+v", th)
	}
	d, err := c.FileDiff(ctx, "proj", "web", 11, "src/app.go")
	if err != nil || d.Patch != "@@ -1,3 +1,3 @@\n package app\n \n-func a() {}\n+func b() {}\n" {
		t.Fatalf("diff = %+v, %v", d, err)
	}
	d, err = c.FileDiff(ctx, "proj", "web", 11, "src/new.go")
	if err != nil || d.Patch != "@@ -0,0 +1 @@\n+package app\n" {
		t.Fatalf("new file = %+v, %v", d, err)
	}
	d, err = c.FileDiff(ctx, "proj", "web", 11, "src/moved.go")
	if err != nil || !strings.Contains(d.Patch, "-x\n+y\n") {
		t.Fatalf("renamed file = %+v, %v", d, err)
	}
	if _, err := c.FileDiff(ctx, "proj", "web", 11, "img.bin"); !errors.Is(err, core.ErrNotFound) || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary file: %v", err)
	}
	if _, err := c.FileDiff(ctx, "proj", "web", 11, "other.go"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unchanged file: %v", err)
	}
	if _, err := c.ReviewThread(ctx, "proj", "web", 11, "x"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad thread ref: %v", err)
	}
}

func TestRequestReviewersAndLogin(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/contoso/_apis/connectionData":
			fmt.Fprint(w, `{"authenticatedUser":{"id":"aaaa-1111","providerDisplayName":"Alice"}}`)
		case r.Method == http.MethodPut:
			path = r.URL.Path
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{Organization: "contoso", BaseURL: srv.URL, Token: "pat"})
	ctx := context.Background()
	id, err := c.Login(ctx)
	if err != nil || id != "aaaa-1111" {
		t.Fatalf("login = %q, %v", id, err)
	}
	if err := c.RequestReviewers(ctx, "proj", "web", 11, []string{id}); err != nil {
		t.Fatal(err)
	}
	if path != "/contoso/proj/_apis/git/repositories/web/pullRequests/11/reviewers/aaaa-1111" || body["vote"] != float64(0) || body["isRequired"] != false {
		t.Fatalf("request: %s %v", path, body)
	}
	if err := c.RequestReviewers(ctx, "proj", "web", 11, []string{" "}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("empty identity: %v", err)
	}
}

// TestNormalizeActionKey shows that the events of one delivery share an
// action key, and that a merge keeps the key of the fact.
func TestNormalizeActionKey(t *testing.T) {
	h := NewWebhookHandler(WebhookOptions{})
	in := core.Integration{TeamID: "t", Config: map[string]string{"org": "contoso"}}
	comment := `{"id":"n1","eventType":"ms.vss-code.git-pullrequest-comment-event","resource":{
		"comment":{"id":2,"content":"hi @payments","author":{"id":"u1","uniqueName":"dana@contoso.com"}},
		"pullRequest":{"pullRequestId":11,"status":"active","title":"T","repository":{"id":"r1","name":"web","project":{"name":"proj"}}}}}`
	evs, err := h.Normalize(in, []byte(comment))
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v err = %v", evs, err)
	}
	for _, ev := range evs {
		if ev.ActionKey != "azure_devops:n1" || ev.DeliveryID != "n1:"+ev.Type {
			t.Errorf("%s: action key = %q, delivery = %q", ev.Type, ev.ActionKey, ev.DeliveryID)
		}
	}
	merged := `{"id":"n2","eventType":"git.pullrequest.merged","resource":{"pullRequestId":11,"status":"completed","title":"T",
		"createdBy":{"id":"u1","uniqueName":"dana@contoso.com"},"repository":{"id":"R1","name":"web","project":{"name":"proj"}}}}`
	evs, err = h.Normalize(in, []byte(merged))
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %v err = %v", evs, err)
	}
	if evs[0].ActionKey != "azure_devops:r1/11:merged" || evs[0].DeliveryID != "r1/11:merged" {
		t.Errorf("merge: action key = %q, delivery = %q", evs[0].ActionKey, evs[0].DeliveryID)
	}
}

type oneForge struct{ f core.Forge }

func (o oneForge) Forge(context.Context, core.ForgeLookup) (core.Forge, error) { return o.f, nil }

// TestReviewBuilderWithAzureDevOps runs the context builder of a review
// comment against a fake Azure DevOps. The payload has no file, so the path
// comes from the thread.
func TestReviewBuilderWithAzureDevOps(t *testing.T) {
	const base = "/contoso/proj/_apis/git/repositories/web"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case base + "/pullRequests/11/threads/66":
			fmt.Fprint(w, `{"id":66,"status":"active","threadContext":{"filePath":"/src/app.go","rightFileStart":{"line":3}},
				"comments":[{"id":1,"content":"rename it","commentType":"text","author":{"uniqueName":"dana@contoso.com"}}]}`)
		case base + "/pullRequests/11/iterations":
			fmt.Fprint(w, `{"value":[{"id":1,"sourceRefCommit":{"commitId":"s1"},"commonRefCommit":{"commitId":"m1"}}]}`)
		case base + "/pullRequests/11/iterations/1/changes":
			fmt.Fprint(w, `{"changeEntries":[{"changeType":"edit","item":{"path":"/src/app.go"}}]}`)
		case base + "/items":
			if r.URL.Query().Get("versionDescriptor.version") == "m1" {
				fmt.Fprint(w, "a\nb\n")
			} else {
				fmt.Fprint(w, "a\nB\n")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{Organization: "contoso", BaseURL: srv.URL, Token: "pat"})
	h := NewWebhookHandler(WebhookOptions{})
	body := `{"id":"n1","eventType":"ms.vss-code.git-pullrequest-comment-event","resource":{
		"comment":{"id":2,"content":"rename it","author":{"id":"u1","uniqueName":"dana@contoso.com"},
			"_links":{"threads":{"href":"https://dev.azure.com/contoso/proj/_apis/git/repositories/r1/pullRequests/11/threads/66"}}},
		"pullRequest":{"pullRequestId":11,"status":"active","title":"T","repository":{"id":"r1","name":"web","project":{"name":"proj"}}}}}`
	evs, err := h.Normalize(core.Integration{TeamID: "t", Config: map[string]string{"org": "contoso"}}, []byte(body))
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v err = %v", evs, err)
	}
	b := eventctx.Build(context.Background(), "m", evs[0], true, eventctx.ReviewBuilders(oneForge{c}))
	files := map[string]string{}
	for _, f := range b.Files {
		files[f.Path] = string(f.Data)
	}
	const dir = ".openfactory/events/m/context/"
	if !strings.Contains(files[dir+"review_thread.json"], "rename it") || !strings.Contains(files[dir+"file_diff.patch"], "-b\n+B\n") {
		t.Fatalf("files = %v, omitted = %+v", b.Envelope.ContextFiles, b.Envelope.Omitted)
	}
}
