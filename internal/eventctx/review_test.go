package eventctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/github"
)

// forgeSource serves one forge client. It records the factory call.
type forgeSource struct {
	f         core.Forge
	err       error
	mu        sync.Mutex
	factoryID string
	repos     []string
}

func (s *forgeSource) Forge(_ context.Context, l core.ForgeLookup) (core.Forge, error) {
	s.mu.Lock()
	s.factoryID, s.repos = l.FactoryID, l.Scope
	s.mu.Unlock()
	return s.f, s.err
}

func reviewEvent() *core.Event {
	return &core.Event{
		ID: "e1", TeamID: "team_1", FactoryID: "fac_1", Provider: "github", Type: "pull_request_commented",
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 7, ThreadRef: "100"},
		Payload: json.RawMessage(`{"comment":{"id":101,"body":"please rename","path":"pkg/x.go","line":4,
			"diff_hunk":"@@ -3,2 +3,2 @@\n-a\n+b","user":{"login":"rev"}}}`),
	}
}

// githubFake serves the review comments and the files of pull request 7.
func githubFake(t *testing.T, status int) *github.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"Resource not accessible"}`)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/api/pulls/7/comments":
			fmt.Fprint(w, `[
			 {"id":100,"body":"rename this","path":"pkg/x.go","line":4,"created_at":"2026-01-02T03:04:05Z","user":{"login":"rev"}},
			 {"id":200,"body":"other thread","path":"pkg/y.go","line":1,"user":{"login":"zed"}},
			 {"id":300,"body":"binary?","path":"big.bin","line":1,"user":{"login":"zed"}},
			 {"id":101,"in_reply_to_id":100,"body":"please rename","path":"pkg/x.go","line":4,"created_at":"2026-01-02T04:00:00Z","user":{"login":"dana"}}]`)
		case "/repos/acme/api/pulls/7/files":
			fmt.Fprint(w, `[{"filename":"pkg/y.go","patch":"@@ -1 +1 @@\n-q\n+r"},
			 {"filename":"pkg/x.go","patch":"@@ -1,5 +1,5 @@\n one\n two\n three\n-four\n+FOUR\n five"},
			 {"filename":"big.bin"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := github.New(github.Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReviewBuilderGitHubReadsThreadAndDiff(t *testing.T) {
	src := &forgeSource{f: githubFake(t, 0)}
	b := Build(context.Background(), "m", reviewEvent(), true, ReviewBuilders(src))
	files := fileMap(b)
	const dir = ".openfactory/events/m/context/"

	var th struct {
		Ref      string
		Path     string
		Comments []core.ThreadComment
	}
	if err := json.Unmarshal([]byte(files[dir+"review_thread.json"]), &th); err != nil {
		t.Fatal(err)
	}
	if th.Ref != "100" || th.Path != "pkg/x.go" || len(th.Comments) != 2 || th.Comments[0].Author != "rev" || th.Comments[1].Author != "dana" {
		t.Fatalf("thread = %+v", th)
	}
	if got := files[dir+"file_diff.patch"]; !strings.HasPrefix(got, "--- a/pkg/x.go\n+++ b/pkg/x.go\n@@ -1,5 +1,5 @@\n") || !strings.Contains(got, "+FOUR") || strings.Contains(got, "-q") {
		t.Fatalf("diff = %q", got)
	}
	// The payload builder still gives the hunk of the comment.
	if !strings.Contains(files[dir+"diff.patch"], "+b") || !strings.Contains(files[dir+"review_comment.json"], "please rename") {
		t.Fatalf("payload files missing: %v", b.Envelope.ContextFiles)
	}
	if len(b.Envelope.Omitted) != 0 {
		t.Fatalf("omitted = %+v", b.Envelope.Omitted)
	}
	// The credential of the factory serves the call, scoped to the repository.
	if src.factoryID != "fac_1" || fmt.Sprint(src.repos) != "[acme/api]" {
		t.Fatalf("factory call = %q %v", src.factoryID, src.repos)
	}
	sec := b.PromptSection()
	if !strings.Contains(sec, "context/review_thread.json") || !strings.Contains(sec, "context/file_diff.patch") {
		t.Fatalf("section: %s", sec)
	}
}

func TestReviewBuilderListsWhatItCannotRead(t *testing.T) {
	ctx := context.Background()
	omitted := func(b *Bundle) map[string]string {
		m := map[string]string{}
		for _, o := range b.Envelope.Omitted {
			m[o.Name] = o.Reason
		}
		return m
	}

	// The forge refuses the credential: both files are left out, the build goes on.
	b := Build(ctx, "m", reviewEvent(), true, ReviewBuilders(&forgeSource{f: githubFake(t, http.StatusForbidden)}))
	o := omitted(b)
	if !strings.Contains(o["context/review_thread.json"], "403") || !strings.Contains(o["context/file_diff.patch"], "403") {
		t.Fatalf("omitted = %+v", b.Envelope.Omitted)
	}
	if _, ok := fileMap(b)[".openfactory/events/m/payload.json"]; !ok {
		t.Fatal("the payload is missing")
	}
	if !strings.Contains(b.PromptSection(), "Left out: context/review_thread.json") {
		t.Fatalf("section: %s", b.PromptSection())
	}

	// A file that GitHub gives no patch for: the thread is there, the diff is left out.
	ev := reviewEvent()
	ev.Source.ThreadRef = "300"
	b = Build(ctx, "m", ev, true, ReviewBuilders(&forgeSource{f: githubFake(t, 0)}))
	o = omitted(b)
	if _, ok := fileMap(b)[".openfactory/events/m/context/review_thread.json"]; !ok {
		t.Fatalf("no thread: %+v", b.Envelope.Omitted)
	}
	if !strings.Contains(o["context/file_diff.patch"], "has no patch") || len(o) != 1 {
		t.Fatalf("omitted = %+v", b.Envelope.Omitted)
	}

	// No forge client for the team.
	b = Build(ctx, "m", reviewEvent(), true, ReviewBuilders(&forgeSource{err: errors.New("no code-forge integration configured")}))
	o = omitted(b)
	if !strings.Contains(o["context/review_thread.json"], "no forge client: no code-forge integration") || len(o) != 2 {
		t.Fatalf("omitted = %+v", b.Envelope.Omitted)
	}

	// A forge that cannot read review context (generic git).
	b = Build(ctx, "m", reviewEvent(), true, ReviewBuilders(&forgeSource{f: plainForge{}}))
	if !strings.Contains(omitted(b)["context/file_diff.patch"], "cannot read review threads or diffs") {
		t.Fatalf("omitted = %+v", b.Envelope.Omitted)
	}

	// A forge that returns operation_not_supported for each call.
	b = Build(ctx, "m", reviewEvent(), true, ReviewBuilders(&forgeSource{f: &readerForge{err: core.ErrOperationNotSupported}}))
	if !strings.Contains(omitted(b)["context/review_thread.json"], "this forge cannot read the review thread") {
		t.Fatalf("omitted = %+v", b.Envelope.Omitted)
	}
}

// A comment without a thread (an issue comment, a top-level comment) does not
// call the forge.
func TestReviewBuilderSkipsOtherEvents(t *testing.T) {
	rf := &readerForge{}
	src := &forgeSource{f: rf}
	for _, ev := range []*core.Event{
		{Provider: "github", Type: "issue_commented", Source: core.SourceRef{Kind: "issue", Repo: "acme/api", Number: 3}},
		{Provider: "github", Type: "pull_request_commented", Source: core.SourceRef{Kind: "pull_request", Repo: "acme/api", Number: 3}},
		{Provider: "gitlab", Type: "note", Source: core.SourceRef{Kind: "issue", Repo: "acme/api", Number: 3, ThreadRef: "d1"}},
		{Provider: "slack", Type: "message", Source: core.SourceRef{ThreadRef: "1.2", Repo: "a/b", Number: 1}},
	} {
		b := Build(context.Background(), "m", ev, true, ReviewBuilders(src))
		if len(b.Envelope.Omitted) != 0 || len(b.Envelope.ContextFiles) != 0 {
			t.Errorf("%s %s: omitted = %+v, files = %v", ev.Provider, ev.Type, b.Envelope.Omitted, b.Envelope.ContextFiles)
		}
	}
	if rf.calls != 0 {
		t.Fatalf("the forge was called %d times", rf.calls)
	}
}

func TestReviewBuilderSizeLimits(t *testing.T) {
	// A diff of about 600 KiB is cut at 256 KiB, at the end of a line.
	var patch strings.Builder
	patch.WriteString("@@ -1,9000 +1,9000 @@\n")
	for patch.Len() < 600<<10 {
		patch.WriteString("+" + strings.Repeat("é", 40) + "\n")
	}
	var comments []core.ThreadComment
	for i := range 8 { // 8 comments of 50 KiB: the older ones do not fit
		comments = append(comments, core.ThreadComment{ID: fmt.Sprint(i), Author: "a", Body: strings.Repeat("x", 50<<10)})
	}
	rf := &readerForge{thread: &core.ReviewThread{Ref: "100", Path: "pkg/x.go", Comments: comments}, diff: &core.FileDiff{Path: "pkg/x.go", Patch: patch.String()}}
	b := Build(context.Background(), "m", reviewEvent(), true, ReviewBuilders(&forgeSource{f: rf}))
	files := fileMap(b)
	const dir = ".openfactory/events/m/context/"
	diff, thread := files[dir+"file_diff.patch"], files[dir+"review_thread.json"]
	if len(diff) > MaxReviewFileBytes || len(diff) < MaxReviewFileBytes-200 || !strings.HasSuffix(diff, "\n") {
		t.Fatalf("diff is %d bytes", len(diff))
	}
	if len(thread) > MaxReviewFileBytes {
		t.Fatalf("thread is %d bytes", len(thread))
	}
	var td struct {
		Comments        []core.ThreadComment
		OmittedComments int `json:"omitted_comments"`
	}
	if err := json.Unmarshal([]byte(thread), &td); err != nil {
		t.Fatal(err)
	}
	if td.OmittedComments == 0 || len(td.Comments)+td.OmittedComments != 8 || td.Comments[len(td.Comments)-1].ID != "7" {
		t.Fatalf("thread has %d comments, %d left out", len(td.Comments), td.OmittedComments)
	}
	reasons := ""
	for _, o := range b.Envelope.Omitted {
		reasons += o.Name + ": " + o.Reason + "\n"
	}
	for _, want := range []string{"file_diff.patch: cut at 256 KiB", "review_thread.json: ", "older comments are left out"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("omitted lacks %q:\n%s", want, reasons)
		}
	}

	// A comment of 1 MiB in a thread of its own is cut.
	rf.thread = &core.ReviewThread{Ref: "100", Comments: []core.ThreadComment{{Body: strings.Repeat("\"", 1<<20)}}}
	data, dropped := threadFile(rf.thread)
	if len(data) > MaxReviewFileBytes || dropped != 0 || !json.Valid(data) {
		t.Fatalf("one large comment: %d bytes, dropped %d, valid %v", len(data), dropped, json.Valid(data))
	}

	// The forge says that it cut the patch.
	rf.diff = &core.FileDiff{Path: "pkg/x.go", Patch: "@@ -1 +1 @@\n-a\n+b", Truncated: true}
	b = Build(context.Background(), "m", reviewEvent(), true, ReviewBuilders(&forgeSource{f: rf}))
	found := false
	for _, o := range b.Envelope.Omitted {
		found = found || (o.Name == "context/file_diff.patch" && o.Reason == "the forge cut the diff")
	}
	if !found {
		t.Fatalf("omitted = %+v", b.Envelope.Omitted)
	}
}

// The files of the builder go through the redactor of the run like the
// payload does.
func TestReviewBuilderFilesAreRedacted(t *testing.T) {
	rf := &readerForge{
		thread: &core.ReviewThread{Ref: "100", Path: "cfg.go", Comments: []core.ThreadComment{{Author: "a", Body: "token is s3cr3t-value"}}},
		diff:   &core.FileDiff{Path: "cfg.go", Patch: "@@ -1 +1 @@\n-x\n+key = \"s3cr3t-value\"\n"},
	}
	b := Build(context.Background(), "m", reviewEvent(), true, ReviewBuilders(&forgeSource{f: rf}))
	raw, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range Redact(got.Files, func(s string) string { return strings.ReplaceAll(s, "s3cr3t-value", "[REDACTED]") }) {
		if strings.Contains(string(f.Data), "s3cr3t-value") {
			t.Fatalf("secret in %s", f.Path)
		}
		if strings.Contains(string(f.Data), "[REDACTED]") {
			seen++
		}
	}
	if seen < 2 {
		t.Fatalf("only %d files had the secret", seen)
	}
}

// plainForge is a core.Forge without review context.
type plainForge struct{ core.Forge }

// readerForge is a core.Forge with canned review context.
type readerForge struct {
	core.Forge
	thread *core.ReviewThread
	diff   *core.FileDiff
	err    error
	calls  int
}

func (f *readerForge) ReviewThread(context.Context, string, string, int, string) (*core.ReviewThread, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.thread, nil
}

func (f *readerForge) FileDiff(context.Context, string, string, int, string) (*core.FileDiff, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.diff, nil
}
