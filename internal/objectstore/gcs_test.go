package objectstore

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeSAEmail = "runner@proj.iam.gserviceaccount.com"

// fakeGCS emulates the OAuth2 token endpoint, the GCE metadata server, the
// IAM signBlob API, the Cloud Storage JSON API and V4 signed URLs.
type fakeGCS struct {
	t        *testing.T
	key      *rsa.PrivateKey
	bucket   string
	clk      *clock
	store    *memStore
	srvURL   string
	tokens   atomic.Int32
	signBlob atomic.Int32
}

const fakeToken = "ya29.fake"

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/token" && r.Method == http.MethodPost:
		f.serveToken(w, r)
		return
	case strings.HasPrefix(p, "/computeMetadata/v1/"):
		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)
			return
		}
		switch strings.TrimPrefix(p, "/computeMetadata/v1/") {
		case "instance/service-accounts/default/token":
			f.tokens.Add(1)
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3599,"token_type":"Bearer"}`, fakeToken)
		case "instance/service-accounts/default/email":
			fmt.Fprint(w, fakeSAEmail)
		default:
			http.NotFound(w, r)
		}
		return
	case r.URL.Query().Get("X-Goog-Signature") != "":
		if reason := f.verifySigned(r); reason != "" {
			http.Error(w, reason, http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(p, "/"+f.bucket+"/")
		f.serveObject(w, r, key, r.Method == http.MethodPut)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+fakeToken {
		http.Error(w, `{"error":{"code":401}}`, http.StatusUnauthorized)
		return
	}
	switch {
	case strings.HasPrefix(p, "/v1/projects/-/serviceAccounts/") && strings.HasSuffix(p, ":signBlob"):
		f.signBlob.Add(1)
		var in struct{ Payload string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		data, _ := base64.StdEncoding.DecodeString(in.Payload)
		sum := sha256.Sum256(data)
		sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
		fmt.Fprintf(w, `{"keyId":"k1","signedBlob":%q}`, base64.StdEncoding.EncodeToString(sig))
	case p == "/upload/storage/v1/b/"+f.bucket+"/o" && r.Method == http.MethodPost:
		if r.URL.Query().Get("uploadType") != "media" {
			http.Error(w, "uploadType", 400)
			return
		}
		name := r.URL.Query().Get("name")
		b, _ := io.ReadAll(r.Body)
		if err := f.store.Put(r.Context(), name, bytes.NewReader(b), int64(len(b)), r.Header.Get("Content-Type")); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		info, _ := f.store.Stat(r.Context(), name)
		_ = json.NewEncoder(w).Encode(f.resource(info))
	case p == "/storage/v1/b/"+f.bucket+"/o" && r.Method == http.MethodGet:
		f.serveList(w, r)
	case strings.HasPrefix(r.URL.EscapedPath(), "/storage/v1/b/"+f.bucket+"/o/"):
		raw := strings.TrimPrefix(r.URL.EscapedPath(), "/storage/v1/b/"+f.bucket+"/o/")
		if strings.Contains(raw, "/") {
			http.Error(w, "object name must be escaped", 400)
			return
		}
		name, _ := url.PathUnescape(raw)
		switch {
		case r.Method == http.MethodDelete:
			if _, err := f.store.Stat(r.Context(), name); err != nil {
				http.Error(w, "nf", 404)
				return
			}
			_ = f.store.Delete(r.Context(), name)
			w.WriteHeader(204)
		case r.URL.Query().Get("alt") == "media":
			f.serveObject(w, r, name, false)
		default:
			info, err := f.store.Stat(r.Context(), name)
			if err != nil {
				http.Error(w, `{"error":{"code":404}}`, 404)
				return
			}
			_ = json.NewEncoder(w).Encode(f.resource(info))
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGCS) resource(o ObjectInfo) map[string]any {
	return map[string]any{"name": o.Key, "size": strconv.FormatInt(o.Size, 10), "contentType": o.ContentType, "updated": o.LastModified.Format(time.RFC3339Nano), "etag": o.ETag}
}

func (f *fakeGCS) serveObject(w http.ResponseWriter, r *http.Request, key string, put bool) {
	if put {
		b, _ := io.ReadAll(r.Body)
		_ = f.store.Put(r.Context(), key, bytes.NewReader(b), int64(len(b)), r.Header.Get("Content-Type"))
		return
	}
	rc, info, err := f.store.Get(r.Context(), key)
	if err != nil {
		http.Error(w, "No such object", 404)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Last-Modified", info.LastModified.Format(http.TimeFormat))
	_, _ = io.Copy(w, rc)
}

func (f *fakeGCS) serveList(w http.ResponseWriter, r *http.Request) {
	all, _ := f.store.List(r.Context(), r.URL.Query().Get("prefix"))
	start, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
	end := min(start+2, len(all))
	out := map[string]any{"kind": "storage#objects"}
	var items []map[string]any
	for _, o := range all[start:end] {
		items = append(items, f.resource(o))
	}
	out["items"] = items
	if end < len(all) {
		out["nextPageToken"] = strconv.Itoa(end)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeGCS) serveToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
		return
	}
	parts := strings.Split(r.Form.Get("assertion"), ".")
	if len(parts) != 3 {
		http.Error(w, `{"error":"invalid_grant"}`, 400)
		return
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		http.Error(w, `{"error":"invalid_grant","error_description":"bad signature"}`, 400)
		return
	}
	var hdr struct{ Alg, Kid string }
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(hb, &hdr)
	var c struct {
		Iss, Scope, Aud string
		Iat, Exp        int64
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(cb, &c)
	now := f.clk.Now().Unix()
	if hdr.Alg != "RS256" || hdr.Kid != "kid-1" || c.Iss != fakeSAEmail || c.Aud != f.srvURL+"/token" ||
		!strings.Contains(c.Scope, "devstorage.read_write") || c.Iat > now || c.Exp <= now {
		http.Error(w, `{"error":"invalid_grant"}`, 400)
		return
	}
	f.tokens.Add(1)
	fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600,"token_type":"Bearer"}`, fakeToken)
}

// verifySigned independently checks a V4 signed URL.
func (f *fakeGCS) verifySigned(r *http.Request) string {
	q := r.URL.Query()
	sig, err := hex.DecodeString(q.Get("X-Goog-Signature"))
	if err != nil {
		return "bad signature encoding"
	}
	q.Del("X-Goog-Signature")
	date := q.Get("X-Goog-Date")
	at, err := time.Parse("20060102T150405Z", date)
	if err != nil {
		return "bad date"
	}
	exp, _ := strconv.Atoi(q.Get("X-Goog-Expires"))
	if f.clk.Now().After(at.Add(time.Duration(exp) * time.Second)) {
		return "ExpiredToken"
	}
	scope := date[:8] + "/auto/storage/goog4_request"
	if q.Get("X-Goog-Credential") != fakeSAEmail+"/"+scope || q.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" {
		return "bad credential"
	}
	var keys []string
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qs []string
	for _, k := range keys {
		qs = append(qs, url.QueryEscape(k)+"="+strings.ReplaceAll(url.QueryEscape(q.Get(k)), "+", "%20"))
	}
	signed := q.Get("X-Goog-SignedHeaders")
	var hdrs string
	for _, h := range strings.Split(signed, ";") {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		hdrs += h + ":" + v + "\n"
	}
	creq := strings.Join([]string{r.Method, r.URL.EscapedPath(), strings.Join(qs, "&"), hdrs, signed, "UNSIGNED-PAYLOAD"}, "\n")
	h := sha256.Sum256([]byte(creq))
	sts := "GOOG4-RSA-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	sum := sha256.Sum256([]byte(sts))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return "SignatureDoesNotMatch"
	}
	return ""
}

func newGCSFixture(t *testing.T, withKey bool) (Store, *fakeGCS, *clock) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	clk := newClock()
	f := &fakeGCS{t: t, key: key, bucket: "bkt", clk: clk, store: newMemStore(clk.Now)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.srvURL = srv.URL
	opts := Options{HTTPClient: srv.Client(), Now: clk.Now}
	env := map[string]string{}
	if withKey {
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		sa, _ := json.Marshal(map[string]string{
			"type":           "service_account",
			"client_email":   fakeSAEmail,
			"private_key_id": "kid-1",
			"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
			"token_uri":      srv.URL + "/token",
		})
		opts.GCSCredentialsJSON = sa
	} else {
		env["GCE_METADATA_HOST"] = strings.TrimPrefix(srv.URL, "http://")
	}
	opts.Getenv = func(k string) string { return env[k] }
	raw := "gs://bkt/pfx?endpoint=" + url.QueryEscape(srv.URL) + "&iam_endpoint=" + url.QueryEscape(srv.URL)
	s, err := Open(context.Background(), raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, clk
}

func TestGCSConformanceServiceAccountKey(t *testing.T) {
	s, f, clk := newGCSFixture(t, true)
	conformance(t, s, http.DefaultClient, clk, true)
	if n := f.tokens.Load(); n != 1 {
		t.Errorf("token fetched %d times, want 1 (cached)", n)
	}
	if f.signBlob.Load() != 0 {
		t.Error("signBlob used despite a local key")
	}
	for _, k := range f.store.keys() {
		if !strings.HasPrefix(k, "pfx/") {
			t.Errorf("object %q outside prefix", k)
		}
	}
}

func TestGCSConformanceMetadataServer(t *testing.T) {
	s, f, clk := newGCSFixture(t, false)
	conformance(t, s, http.DefaultClient, clk, true)
	if f.signBlob.Load() == 0 {
		t.Error("signed URLs did not use IAM signBlob")
	}
}

func TestGCSTokenRefresh(t *testing.T) {
	s, f, clk := newGCSFixture(t, true)
	ctx := context.Background()
	if _, err := s.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	clk.Advance(30 * time.Minute)
	if _, err := s.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if f.tokens.Load() != 1 {
		t.Fatalf("token refetched early: %d", f.tokens.Load())
	}
	clk.Advance(30 * time.Minute) // within a minute of expiry
	if _, err := s.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if f.tokens.Load() != 2 {
		t.Fatalf("token not refreshed: %d", f.tokens.Load())
	}
}

func TestGCSOpenErrors(t *testing.T) {
	none := func(string) string { return "" }
	for name, opts := range map[string]Options{
		"bad json":   {GCSCredentialsJSON: []byte("{"), Getenv: none},
		"user creds": {GCSCredentialsJSON: []byte(`{"type":"authorized_user"}`), Getenv: none},
		"bad key":    {GCSCredentialsJSON: []byte(`{"type":"service_account","private_key":"nope"}`), Getenv: none},
		"missing file": {Getenv: func(k string) string {
			return map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": "/nonexistent/key.json"}[k]
		}},
	} {
		if _, err := Open(context.Background(), "gs://bkt", opts); err == nil {
			t.Errorf("%s: Open succeeded", name)
		}
	}
	if _, err := Open(context.Background(), "gs:///x", Options{Getenv: none}); err == nil {
		t.Error("gs without bucket accepted")
	}
}
