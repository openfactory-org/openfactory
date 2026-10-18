package objectstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeAzure emulates Blob Storage in Azurite's path-style layout
// (http://host/{account}/{container}/{blob}) and verifies Shared Key and
// Service SAS signatures with an independent implementation of the spec.
type fakeAzure struct {
	account, container string
	key                []byte
	clk                *clock
	store              *memStore
}

func (f *fakeAzure) mac(s string) string {
	m := hmac.New(sha256.New, f.key)
	m.Write([]byte(s))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func (f *fakeAzure) verifySharedKey(r *http.Request) string {
	want := "SharedKey " + f.account + ":"
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, want) {
		return "NoAuthenticationInformation"
	}
	if r.Header.Get("x-ms-version") == "" {
		return "MissingRequiredHeader x-ms-version"
	}
	d, err := http.ParseTime(r.Header.Get("x-ms-date"))
	if err != nil || d.Sub(f.clk.Now()).Abs() > 15*time.Minute {
		return "RequestTimeTooSkewed"
	}
	length := r.Header.Get("Content-Length")
	if length == "0" {
		length = ""
	}
	lines := []string{r.Method, r.Header.Get("Content-Encoding"), r.Header.Get("Content-Language"), length,
		r.Header.Get("Content-MD5"), r.Header.Get("Content-Type"), r.Header.Get("Date"), r.Header.Get("If-Modified-Since"),
		r.Header.Get("If-Match"), r.Header.Get("If-None-Match"), r.Header.Get("If-Unmodified-Since"), r.Header.Get("Range")}
	var xms []string
	for k, v := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-ms-") {
			xms = append(xms, lk+":"+strings.TrimSpace(strings.Join(v, ",")))
		}
	}
	sort.Strings(xms)
	res := "/" + f.account + r.URL.EscapedPath()
	q := r.URL.Query()
	var qk []string
	for k := range q {
		qk = append(qk, strings.ToLower(k))
	}
	sort.Strings(qk)
	for _, k := range qk {
		res += "\n" + k + ":" + strings.Join(q[k], ",")
	}
	sts := strings.Join(lines, "\n") + "\n" + strings.Join(xms, "\n") + "\n" + res
	if strings.TrimPrefix(auth, want) != f.mac(sts) {
		return "AuthenticationFailed"
	}
	return ""
}

func (f *fakeAzure) verifySAS(r *http.Request, blob string) string {
	q := r.URL.Query()
	se, err := time.Parse("2006-01-02T15:04:05Z", q.Get("se"))
	if err != nil || f.clk.Now().After(se) {
		return "AuthenticationFailed: expired"
	}
	if st := q.Get("st"); st != "" {
		if t, err := time.Parse("2006-01-02T15:04:05Z", st); err != nil || f.clk.Now().Before(t) {
			return "AuthenticationFailed: not yet valid"
		}
	}
	need := "r"
	if r.Method == http.MethodPut {
		need = "w"
	}
	if q.Get("sr") != "b" || !strings.Contains(q.Get("sp"), need) {
		return "AuthorizationPermissionMismatch"
	}
	cr := "/blob/" + f.account + "/" + f.container + "/" + blob
	sts := q.Get("sp") + "\n" + q.Get("st") + "\n" + q.Get("se") + "\n" + cr + "\n\n\n\n" + q.Get("sv") + "\nb\n\n\n\n\n\n\n"
	if q.Get("sig") != f.mac(sts) {
		return "AuthenticationFailed: signature"
	}
	return ""
}

func (f *fakeAzure) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) < 2 || parts[0] != f.account || parts[1] != f.container {
		http.Error(w, "ContainerNotFound", 404)
		return
	}
	blob := ""
	if len(parts) == 3 {
		blob = parts[2]
	}
	var reason string
	if r.URL.Query().Get("sig") != "" {
		reason = f.verifySAS(r, blob)
	} else {
		reason = f.verifySharedKey(r)
	}
	if reason != "" {
		http.Error(w, reason, http.StatusForbidden)
		return
	}
	ctx := r.Context()
	if blob == "" {
		q := r.URL.Query()
		if q.Get("restype") != "container" || q.Get("comp") != "list" {
			http.Error(w, "bad op", 400)
			return
		}
		f.list(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("x-ms-blob-type") != "BlockBlob" {
			http.Error(w, "MissingRequiredHeader x-ms-blob-type", 400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = f.store.Put(ctx, blob, bytes.NewReader(b), int64(len(b)), r.Header.Get("Content-Type"))
		w.WriteHeader(201)
	case http.MethodGet, http.MethodHead:
		rc, info, err := f.store.Get(ctx, blob)
		if err != nil {
			http.Error(w, "BlobNotFound", 404)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", info.ContentType)
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.Header().Set("Last-Modified", info.LastModified.Format(http.TimeFormat))
		w.Header().Set("ETag", `"0x`+info.ETag+`"`)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, rc)
		}
	case http.MethodDelete:
		if _, err := f.store.Stat(ctx, blob); err != nil {
			http.Error(w, "BlobNotFound", 404)
			return
		}
		_ = f.store.Delete(ctx, blob)
		w.WriteHeader(202)
	}
}

func (f *fakeAzure) list(w http.ResponseWriter, r *http.Request) {
	all, _ := f.store.List(r.Context(), r.URL.Query().Get("prefix"))
	start, _ := strconv.Atoi(r.URL.Query().Get("marker"))
	end := min(start+2, len(all))
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><EnumerationResults><Blobs>`)
	for _, o := range all[start:end] {
		var name bytes.Buffer
		_ = xml.EscapeText(&name, []byte(o.Key))
		fmt.Fprintf(&b, "<Blob><Name>%s</Name><Properties><Last-Modified>%s</Last-Modified><Etag>0x%s</Etag><Content-Length>%d</Content-Length><Content-Type>%s</Content-Type></Properties></Blob>",
			name.String(), o.LastModified.Format(http.TimeFormat), o.ETag, o.Size, o.ContentType)
	}
	b.WriteString("</Blobs><NextMarker>")
	if end < len(all) {
		b.WriteString(strconv.Itoa(end))
	}
	b.WriteString("</NextMarker></EnumerationResults>")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}

func TestAzureConformance(t *testing.T) {
	clk := newClock()
	key := []byte("azure-test-account-key-32-bytes!")
	f := &fakeAzure{account: "devacct", container: "runs", key: key, clk: clk, store: newMemStore(clk.Now)}
	srv := httptest.NewServer(f)
	defer srv.Close()
	env := map[string]string{"AZURE_STORAGE_KEY": base64.StdEncoding.EncodeToString(key)}
	raw := "azblob://devacct/runs/tenant?endpoint=" + url.QueryEscape(srv.URL+"/devacct")
	s, err := Open(context.Background(), raw, Options{HTTPClient: srv.Client(), Now: clk.Now, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	conformance(t, s, srv.Client(), clk, true)
	for _, k := range f.store.keys() {
		if !strings.HasPrefix(k, "tenant/") {
			t.Errorf("blob %q outside prefix", k)
		}
	}
	if h := PutHeaders(s, "text/plain"); h.Get("x-ms-blob-type") != "BlockBlob" || h.Get("Content-Type") != "text/plain" {
		t.Errorf("PutHeaders = %v", h)
	}
}

func TestAzureDefaultEndpointAndErrors(t *testing.T) {
	none := func(string) string { return "" }
	k := base64.StdEncoding.EncodeToString([]byte("k"))
	s, err := Open(context.Background(), "azblob://acct/cont", Options{AzureAccountKey: k, Getenv: none})
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.PresignGet(context.Background(), "a b.txt", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, "https://acct.blob.core.windows.net/cont/a%20b.txt?") || !strings.Contains(u, "sp=r") {
		t.Errorf("SAS URL %s", u)
	}
	for _, raw := range []string{"azblob://acct", "azblob:///cont"} {
		if _, err := Open(context.Background(), raw, Options{AzureAccountKey: k, Getenv: none}); err == nil {
			t.Errorf("Open(%q) succeeded", raw)
		}
	}
	if _, err := Open(context.Background(), "azblob://acct/cont", Options{Getenv: none}); err == nil {
		t.Error("Open without key succeeded")
	}
	if _, err := Open(context.Background(), "azblob://acct/cont", Options{AzureAccountKey: "%%%", Getenv: none}); err == nil {
		t.Error("Open with non-base64 key succeeded")
	}
}
