package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestReviewThreadAndFileDiffFollowPages shows that the thread and the diff
// are found on the second page of the lists.
func TestReviewThreadAndFileDiffFollowPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch r.URL.Path {
		case "/repos/acme/api/pulls/7/comments":
			if page == "1" {
				var parts []string
				for i := 1; i <= 100; i++ {
					parts = append(parts, fmt.Sprintf(`{"id":%d,"body":"c","path":"f.go","line":1,"user":{"login":"u"}}`, 1000+i))
				}
				fmt.Fprint(w, "["+strings.Join(parts, ",")+"]")
				return
			}
			fmt.Fprint(w, `[{"id":5,"body":"root","path":"pkg/x.go","original_line":9,"created_at":"2026-01-02T03:04:05Z","user":{"login":"rev"}},
				{"id":6,"in_reply_to_id":5,"body":"reply","path":"pkg/x.go","line":9,"user":{"login":"dana"}}]`)
		case "/repos/acme/api/pulls/7/files":
			if page == "1" {
				var parts []string
				for i := 1; i <= 100; i++ {
					parts = append(parts, fmt.Sprintf(`{"filename":"f%d.go","patch":"@@"}`, i))
				}
				fmt.Fprint(w, "["+strings.Join(parts, ",")+"]")
				return
			}
			fmt.Fprint(w, `[{"filename":"pkg/x.go","patch":"@@ -1 +1 @@\n-a\n+b"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	th, err := c.ReviewThread(ctx, "acme", "api", 7, "5")
	if err != nil {
		t.Fatal(err)
	}
	// The original line stands in for a line that GitHub no longer maps.
	if th.Ref != "5" || th.Path != "pkg/x.go" || th.Line != 9 || len(th.Comments) != 2 || th.Comments[1].Body != "reply" || th.Comments[0].CreatedAt.IsZero() {
		t.Fatalf("thread = %+v", th)
	}
	if _, err := c.ReviewThread(ctx, "acme", "api", 7, "999"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown thread: %v", err)
	}
	if _, err := c.ReviewThread(ctx, "acme", "api", 7, "abc"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad ref: %v", err)
	}
	d, err := c.FileDiff(ctx, "acme", "api", 7, "/pkg/x.go")
	if err != nil || d.Path != "pkg/x.go" || !strings.Contains(d.Patch, "+b") {
		t.Fatalf("diff = %+v, %v", d, err)
	}
	if _, err := c.FileDiff(ctx, "acme", "api", 7, "none.go"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unchanged file: %v", err)
	}
}
