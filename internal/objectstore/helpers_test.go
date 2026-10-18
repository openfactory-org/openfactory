package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// clock is a settable test clock shared by stores and fake servers.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// memStore is an in-memory Store used by RunData tests and as the backend of
// the fake cloud servers.
type memStore struct {
	mu   sync.Mutex
	now  func() time.Time
	objs map[string]memObj
}

type memObj struct {
	data []byte
	ct   string
	mod  time.Time
}

func newMemStore(now func() time.Time) *memStore {
	return &memStore{now: now, objs: map[string]memObj{}}
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader, size int64, ct string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(b)) != size {
		return fmt.Errorf("size mismatch %d != %d", len(b), size)
	}
	m.mu.Lock()
	m.objs[key] = memObj{data: b, ct: ct, mod: m.now()}
	m.mu.Unlock()
	return nil
}

func (m *memStore) info(k string, o memObj) ObjectInfo {
	return ObjectInfo{Key: k, Size: int64(len(o.data)), ContentType: o.ct, LastModified: o.mod, ETag: fmt.Sprintf("e%d", len(o.data))}
}

func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, ObjectInfo{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data)), m.info(key, o), nil
}

func (m *memStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	_, info, err := m.Get(ctx, key)
	return info, err
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.objs, key)
	m.mu.Unlock()
	return nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ObjectInfo
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, m.info(k, o))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "mem://" + key, nil
}

func (m *memStore) PresignPut(_ context.Context, key string, _ time.Duration, _ string) (string, error) {
	return "mem://" + key + "?put", nil
}

func (m *memStore) setMod(key string, t time.Time) {
	m.mu.Lock()
	o := m.objs[key]
	o.mod = t
	m.objs[key] = o
	m.mu.Unlock()
}

func (m *memStore) keys() []string {
	l, _ := m.List(context.Background(), "")
	var ks []string
	for _, o := range l {
		ks = append(ks, o.Key)
	}
	return ks
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// conformance exercises the Store contract. client must reach presigned
// URLs; clk drives expiry.
func conformance(t *testing.T, s Store, client *http.Client, clk *clock, listsContentType bool) {
	t.Helper()
	ctx := context.Background()
	mustPut := func(key, body, ct string, size int64) {
		t.Helper()
		if err := s.Put(ctx, key, strings.NewReader(body), size, ct); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
	}
	mustPut("a/b.txt", "hello", "text/plain", 5)
	mustPut("a/c/d.json", `{"x":1}`, "application/json", -1)
	mustPut("empty", "", "", 0)
	odd := "dir/with space+plus&ü=.txt"
	mustPut(odd, "odd", "text/plain", 3)

	t.Run("get", func(t *testing.T) {
		rc, info, err := s.Get(ctx, "a/b.txt")
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, rc); got != "hello" {
			t.Fatalf("body %q", got)
		}
		if info.Size != 5 || info.ContentType != "text/plain" {
			t.Fatalf("info %+v", info)
		}
		rc, _, err = s.Get(ctx, odd)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, rc); got != "odd" {
			t.Fatalf("odd body %q", got)
		}
		rc, _, err = s.Get(ctx, "empty")
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, rc); got != "" {
			t.Fatalf("empty body %q", got)
		}
	})
	t.Run("stat", func(t *testing.T) {
		info, err := s.Stat(ctx, "a/c/d.json")
		if err != nil {
			t.Fatal(err)
		}
		if info.Key != "a/c/d.json" || info.Size != 7 || info.ContentType != "application/json" {
			t.Fatalf("info %+v", info)
		}
	})
	t.Run("list", func(t *testing.T) {
		for _, tc := range []struct {
			prefix string
			want   []string
		}{
			{"a/", []string{"a/b.txt", "a/c/d.json"}},
			{"a/b", []string{"a/b.txt"}},
			{"", []string{"a/b.txt", "a/c/d.json", "dir/with space+plus&ü=.txt", "empty"}},
			{"nope/", nil},
		} {
			got, err := s.List(ctx, tc.prefix)
			if err != nil {
				t.Fatalf("List(%q): %v", tc.prefix, err)
			}
			var keys []string
			for _, o := range got {
				keys = append(keys, o.Key)
			}
			if strings.Join(keys, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("List(%q) = %v, want %v", tc.prefix, keys, tc.want)
			}
			if tc.prefix == "a/" && got[0].Size != 5 {
				t.Fatalf("list size %+v", got[0])
			}
			if tc.prefix == "a/" && listsContentType && got[0].ContentType != "text/plain" {
				t.Fatalf("list content type %+v", got[0])
			}
		}
	})
	t.Run("not found", func(t *testing.T) {
		if _, _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get missing: %v", err)
		}
		if _, err := s.Stat(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Stat missing: %v", err)
		}
		if err := s.Delete(ctx, "missing"); err != nil {
			t.Fatalf("Delete missing: %v", err)
		}
	})
	t.Run("invalid keys", func(t *testing.T) {
		for _, k := range []string{"", "../x", "a/../../b", "/abs", "a//b", "a\\b", "a/./b"} {
			if err := s.Put(ctx, k, strings.NewReader("x"), 1, ""); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("Put(%q) = %v, want ErrInvalidKey", k, err)
			}
			if _, err := s.PresignGet(ctx, k, time.Minute); !errors.Is(err, ErrInvalidKey) {
				t.Errorf("PresignGet(%q) = %v, want ErrInvalidKey", k, err)
			}
		}
	})
	t.Run("presign get", func(t *testing.T) {
		for _, key := range []string{"a/b.txt", odd} {
			u, err := s.PresignGet(ctx, key, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Get(u)
			if err != nil {
				t.Fatal(err)
			}
			body := readAll(t, resp.Body)
			if resp.StatusCode != 200 {
				t.Fatalf("presigned GET %s: %d %s", key, resp.StatusCode, body)
			}
			if want := map[string]string{"a/b.txt": "hello", odd: "odd"}[key]; body != want {
				t.Fatalf("presigned body %q", body)
			}
		}
	})
	t.Run("presign tamper and expiry", func(t *testing.T) {
		u, err := s.PresignGet(ctx, "a/b.txt", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		// Swap the key: the signature must not transfer to another object.
		tampered := strings.Replace(u, "b.txt", "c/d.json", 1)
		if resp, err := client.Get(tampered); err != nil {
			t.Fatal(err)
		} else if resp.Body.Close(); resp.StatusCode == 200 {
			t.Fatal("tampered URL accepted")
		}
		clk.Advance(2 * time.Minute)
		defer clk.Advance(-2 * time.Minute)
		resp, err := client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("expired URL accepted")
		}
	})
	t.Run("presign put", func(t *testing.T) {
		u, err := s.PresignPut(ctx, "up/file.bin", time.Minute, "application/pdf")
		if err != nil {
			t.Fatal(err)
		}
		put := func(ct string) int {
			req, _ := http.NewRequest(http.MethodPut, u, strings.NewReader("%PDF"))
			for k, v := range PutHeaders(s, "application/pdf") {
				req.Header[k] = v
			}
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				t.Logf("PUT %d: %s", resp.StatusCode, b)
			}
			return resp.StatusCode
		}
		if code := put(""); code/100 != 2 {
			t.Fatalf("presigned PUT status %d", code)
		}
		rc, info, err := s.Get(ctx, "up/file.bin")
		if err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, rc); got != "%PDF" || info.ContentType != "application/pdf" {
			t.Fatalf("after PUT: %q %+v", got, info)
		}
		// A GET URL must not authorize a PUT.
		gu, _ := s.PresignGet(ctx, "up/file.bin", time.Minute)
		req, _ := http.NewRequest(http.MethodPut, gu, strings.NewReader("evil"))
		for k, v := range PutHeaders(s, "") {
			req.Header[k] = v
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			t.Fatal("GET URL accepted for PUT")
		}
	})
	t.Run("delete", func(t *testing.T) {
		if err := s.Delete(ctx, "a/b.txt"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Get(ctx, "a/b.txt"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
	})
}
