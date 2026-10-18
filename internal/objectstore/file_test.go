package objectstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newFileFixture(t *testing.T) (*FileStore, *httptest.Server, *clock, string) {
	t.Helper()
	clk := newClock()
	dir := t.TempDir()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s, err := Open(context.Background(), "file://"+filepath.ToSlash(dir), Options{SigningKey: []byte("k3y"), BaseURL: srv.URL, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	fs := s.(*FileStore)
	fs.Handler().Register(mux)
	return fs, srv, clk, dir
}

func TestFileConformance(t *testing.T) {
	s, srv, clk, _ := newFileFixture(t)
	conformance(t, s, srv.Client(), clk, true)
}

func TestValidateKey(t *testing.T) {
	for _, tc := range []struct {
		key string
		ok  bool
	}{
		{"a", true},
		{"runs/1/artifacts/screen shot.png", true},
		{"ü/日本", true},
		{"", false},
		{"/etc/passwd", false},
		{"../etc/passwd", false},
		{"a/../../etc", false},
		{"a/./b", false},
		{"a//b", false},
		{"a/", false},
		{"..", false},
		{"a\\..\\b", false},
		{"a\x00b", false},
		{"a\nb", false},
		{strings.Repeat("x", 1025), false},
	} {
		err := ValidateKey(tc.key)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateKey(%q) = %v, want ok=%v", tc.key, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidateKey(%q) error not ErrInvalidKey: %v", tc.key, err)
		}
	}
}

func TestFileStoreStaysInRoot(t *testing.T) {
	s, _, _, dir := newFileFixture(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(dir), "escaped.txt")
	for _, k := range []string{"../escaped.txt", "a/../../escaped.txt"} {
		if err := s.Put(ctx, k, strings.NewReader("x"), 1, ""); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Put(%q) = %v", k, err)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("file written outside root")
	}
	// The metadata directory is reserved and hidden from listings.
	if err := s.Put(ctx, ".ofmeta/x", strings.NewReader("x"), 1, ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("reserved key accepted: %v", err)
	}
	if err := s.Put(ctx, "k", strings.NewReader("v"), 1, "text/x"); err != nil {
		t.Fatal(err)
	}
	l, _ := s.List(ctx, "")
	if len(l) != 1 || l[0].Key != "k" {
		t.Fatalf("list = %+v", l)
	}
	// Short bodies are rejected and leave nothing behind.
	if err := s.Put(ctx, "short", strings.NewReader("ab"), 5, ""); err == nil {
		t.Fatal("short body accepted")
	}
	if _, err := s.Stat(ctx, "short"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("short object exists: %v", err)
	}
	// Deleting removes empty parent directories.
	_ = s.Put(ctx, "deep/er/x", strings.NewReader("1"), 1, "")
	_ = s.Delete(ctx, "deep/er/x")
	if _, err := os.Stat(filepath.Join(dir, "deep")); !os.IsNotExist(err) {
		t.Fatalf("empty dirs left behind: %v", err)
	}
}

func TestFileHandler(t *testing.T) {
	s, srv, clk, _ := newFileFixture(t)
	ctx := context.Background()
	if err := s.Put(ctx, "runs/r1/artifacts/log.txt", strings.NewReader("0123456789"), 10, "text/plain"); err != nil {
		t.Fatal(err)
	}
	good, err := s.PresignGet(ctx, "runs/r1/artifacts/log.txt", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(good, srv.URL+"/objects/runs/r1/artifacts/log.txt?") {
		t.Fatalf("URL %s", good)
	}
	gu, _ := url.Parse(good)
	q := gu.Query()

	do := func(method, rawURL string, hdr map[string]string, body string) (int, string, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(method, rawURL, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b), resp.Header
	}
	withQuery := func(mut func(url.Values)) string {
		v := url.Values{}
		for k, vs := range q {
			v[k] = append([]string(nil), vs...)
		}
		mut(v)
		u := *gu
		u.RawQuery = v.Encode()
		return u.String()
	}

	t.Run("range and headers", func(t *testing.T) {
		code, body, h := do("GET", good, map[string]string{"Range": "bytes=2-4"}, "")
		if code != http.StatusPartialContent || body != "234" {
			t.Fatalf("range: %d %q", code, body)
		}
		if h.Get("Cache-Control") != "private, no-store" || h.Get("Content-Type") != "text/plain" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("headers %v", h)
		}
	})
	t.Run("head", func(t *testing.T) {
		if code, _, _ := do("HEAD", good, nil, ""); code != 200 {
			t.Fatalf("HEAD %d", code)
		}
	})
	for _, tc := range []struct {
		name string
		url  string
		want int
	}{
		{"bad sig", withQuery(func(v url.Values) { v.Set("sig", "AAAA") }), 403},
		{"missing sig", withQuery(func(v url.Values) { v.Del("sig") }), 403},
		{"extended exp", withQuery(func(v url.Values) { v.Set("exp", "99999999999") }), 403},
		{"bad exp", withQuery(func(v url.Values) { v.Set("exp", "soon") }), 403},
		{"other key", strings.Replace(good, "log.txt", "other.txt", 1), 403},
		{"dotdot", srv.URL + "/objects/runs/%2e%2e/%2e%2e/etc/passwd?" + gu.RawQuery, 400},
		{"encoded slash dotdot", srv.URL + "/objects/a/..%2f..%2fx?" + gu.RawQuery, 400},
		{"backslash", srv.URL + "/objects/a%5c..%5cx?" + gu.RawQuery, 400},
		{"reserved meta", srv.URL + "/objects/.ofmeta/runs/r1/artifacts/log.txt.json?" + gu.RawQuery, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body, _ := do("GET", tc.url, nil, ""); code != tc.want {
				t.Fatalf("status %d (%s), want %d", code, body, tc.want)
			}
		})
	}
	t.Run("expired", func(t *testing.T) {
		clk.Advance(61 * time.Second)
		defer clk.Advance(-61 * time.Second)
		if code, body, _ := do("GET", good, nil, ""); code != 403 || !strings.Contains(body, "expired") {
			t.Fatalf("expired: %d %s", code, body)
		}
	})
	t.Run("put", func(t *testing.T) {
		pu, err := s.PresignPut(ctx, "runs/r1/attachments/in.csv", time.Minute, "text/csv")
		if err != nil {
			t.Fatal(err)
		}
		if code, _, _ := do("PUT", pu, map[string]string{"Content-Type": "text/html"}, "a,b"); code != 403 {
			t.Fatalf("wrong content type accepted: %d", code)
		}
		if code, _, _ := do("PUT", pu, nil, "a,b"); code != 403 {
			t.Fatalf("missing content type accepted: %d", code)
		}
		// Dropping ct from the URL breaks the signature.
		pv, _ := url.Parse(pu)
		pq := pv.Query()
		pq.Del("ct")
		pv.RawQuery = pq.Encode()
		if code, _, _ := do("PUT", pv.String(), map[string]string{"Content-Type": "text/html"}, "a,b"); code != 403 {
			t.Fatalf("unsigned content type accepted: %d", code)
		}
		if code, _, _ := do("PUT", pu, map[string]string{"Content-Type": "text/csv"}, "a,b"); code != 200 {
			t.Fatalf("PUT: %d", code)
		}
		info, err := s.Stat(ctx, "runs/r1/attachments/in.csv")
		if err != nil || info.Size != 3 || info.ContentType != "text/csv" {
			t.Fatalf("stat %+v %v", info, err)
		}
		// A PUT URL does not grant GET.
		if code, _, _ := do("GET", pu, nil, ""); code != 403 {
			t.Fatalf("PUT URL allowed GET: %d", code)
		}
	})
	t.Run("put too large", func(t *testing.T) {
		small, err := NewFileStore(t.TempDir(), Options{SigningKey: []byte("k"), MaxPutBytes: 4, Now: clk.Now})
		if err != nil {
			t.Fatal(err)
		}
		pu, _ := small.PresignPut(ctx, "big", time.Minute, "")
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", pu, strings.NewReader("123456789"))
		small.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d", rec.Code)
		}
	})
}

func TestFilePresignWithoutKey(t *testing.T) {
	s, err := NewFileStore(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PresignGet(context.Background(), "k", time.Minute); !errors.Is(err, ErrPresignUnsupported) {
		t.Fatalf("err = %v", err)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/objects/k?exp=1&sig=x", nil))
	if rec.Code != 403 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestOpenFileURLs(t *testing.T) {
	dir := t.TempDir()
	for _, raw := range []string{"file://" + dir, "file://localhost" + dir, "file://" + dir + "?x=1"} {
		s, err := Open(context.Background(), raw, Options{})
		if err != nil {
			t.Fatalf("Open(%q): %v", raw, err)
		}
		if s.(*FileStore).Root() != dir {
			t.Errorf("root %s, want %s", s.(*FileStore).Root(), dir)
		}
	}
	for _, raw := range []string{"file://relative/path", "nonsense", "ftp://x/y"} {
		if _, err := Open(context.Background(), raw, Options{}); err == nil {
			t.Errorf("Open(%q) succeeded", raw)
		}
	}
}
