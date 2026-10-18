package objectstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeS3 emulates the S3 REST API subset the driver uses and verifies SigV4
// header and query authentication.
type fakeS3 struct {
	t         *testing.T
	bucket    string
	creds     awsCreds
	region    string
	clk       *clock
	store     *memStore
	pathStyle bool
	pageSize  int
}

func (f *fakeS3) deny(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, "<Error><Code>AccessDenied</Code><Message>%s</Message></Error>", msg)
}

// verify checks header or presigned authentication. It returns "" on
// success or a reason.
func (f *fakeS3) verify(r *http.Request) string {
	q := r.URL.Query()
	signer := sigV4{f.creds, f.region, "s3"}
	var signedHeaders, sig, amzDate, payload string
	presigned := q.Get("X-Amz-Signature") != ""
	if presigned {
		sig = q.Get("X-Amz-Signature")
		q.Del("X-Amz-Signature")
		signedHeaders = q.Get("X-Amz-SignedHeaders")
		amzDate = q.Get("X-Amz-Date")
		payload = unsignedPayload
		if q.Get("X-Amz-Credential") != f.creds.AccessKeyID+"/"+amzDate[:8]+"/"+f.region+"/s3/aws4_request" {
			return "bad credential scope"
		}
		t, err := time.Parse(amzDateFormat, amzDate)
		exp, _ := strconv.Atoi(q.Get("X-Amz-Expires"))
		if err != nil || f.clk.Now().After(t.Add(time.Duration(exp)*time.Second)) {
			return "Request has expired"
		}
	} else {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
			return "missing auth"
		}
		for _, part := range strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ", ") {
			k, v, _ := strings.Cut(part, "=")
			switch k {
			case "SignedHeaders":
				signedHeaders = v
			case "Signature":
				sig = v
			}
		}
		amzDate = r.Header.Get("X-Amz-Date")
		payload = r.Header.Get("X-Amz-Content-Sha256")
		if f.creds.SessionToken != "" && r.Header.Get("X-Amz-Security-Token") != f.creds.SessionToken {
			return "bad session token"
		}
	}
	at, err := time.Parse(amzDateFormat, amzDate)
	if err != nil {
		return "bad date"
	}
	var hdrs strings.Builder
	for _, h := range strings.Split(signedHeaders, ";") {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		hdrs.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	creq := strings.Join([]string{r.Method, awsURIEncode(r.URL.Path, true), canonicalQuery(q), hdrs.String(), signedHeaders, payload}, "\n")
	if signer.signature(at, creq) != sig {
		return "SignatureDoesNotMatch"
	}
	return ""
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if reason := f.verify(r); reason != "" {
		f.deny(w, reason)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if f.pathStyle {
		b, rest, _ := strings.Cut(path, "/")
		if b != f.bucket {
			http.Error(w, "NoSuchBucket", 404)
			return
		}
		path = rest
	} else if !strings.HasPrefix(r.Host, f.bucket+".") {
		http.Error(w, "NoSuchBucket", 404)
		return
	}
	ctx := r.Context()
	if path == "" && r.Method == http.MethodGet {
		f.list(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(b)) {
			http.Error(w, "length", 400)
			return
		}
		_ = f.store.Put(ctx, path, bytes.NewReader(b), int64(len(b)), r.Header.Get("Content-Type"))
		w.Header().Set("ETag", `"x"`)
	case http.MethodGet, http.MethodHead:
		rc, info, err := f.store.Get(ctx, path)
		if err != nil {
			w.WriteHeader(404)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", info.ContentType)
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.Header().Set("Last-Modified", info.LastModified.Format(http.TimeFormat))
		w.Header().Set("ETag", `"`+info.ETag+`"`)
		if r.Method == http.MethodGet {
			_, _ = io.Copy(w, rc)
		}
	case http.MethodDelete:
		_ = f.store.Delete(ctx, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(405)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("list-type") != "2" {
		http.Error(w, "want v2", 400)
		return
	}
	all, _ := f.store.List(r.Context(), q.Get("prefix"))
	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	end := min(start+f.pageSize, len(all))
	type content struct {
		Key          string
		Size         int64
		LastModified string
		ETag         string
	}
	res := struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
		Contents              []content
	}{IsTruncated: end < len(all)}
	if res.IsTruncated {
		res.NextContinuationToken = strconv.Itoa(end)
	}
	for _, o := range all[start:end] {
		res.Contents = append(res.Contents, content{o.Key, o.Size, o.LastModified.Format("2006-01-02T15:04:05.000Z"), `"` + o.ETag + `"`})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func newS3Fixture(t *testing.T, pathStyle bool, prefix string) (Store, *http.Client, *clock, *fakeS3) {
	clk := newClock()
	f := &fakeS3{t: t, bucket: "bkt", creds: awsCreds{"AKID", "SECRET", "TOKEN"}, region: "eu-west-1", clk: clk, store: newMemStore(clk.Now), pathStyle: pathStyle, pageSize: 2}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	// Route every host (including virtual-hosted bkt.<host>) to the fake.
	addr := srv.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
	host := "s3.test:" + strings.Split(addr, ":")[1]
	raw := fmt.Sprintf("s3://bkt/%s?region=eu-west-1&endpoint=%s&path_style=%t", prefix, url.QueryEscape("http://"+host), pathStyle)
	env := map[string]string{"AWS_ACCESS_KEY_ID": "AKID", "AWS_SECRET_ACCESS_KEY": "SECRET", "AWS_SESSION_TOKEN": "TOKEN"}
	s, err := Open(context.Background(), raw, Options{HTTPClient: client, Now: clk.Now, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	return s, client, clk, f
}

func TestS3Conformance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pathStyle bool
		prefix    string
	}{
		{"path style", true, ""},
		{"virtual host with prefix", false, "tenant/a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client, clk, f := newS3Fixture(t, tc.pathStyle, tc.prefix)
			conformance(t, s, client, clk, false)
			if tc.prefix != "" {
				for _, k := range f.store.keys() {
					if !strings.HasPrefix(k, tc.prefix+"/") {
						t.Errorf("object %q stored outside prefix", k)
					}
				}
			}
		})
	}
}

func TestS3PresignPutEnforcesContentType(t *testing.T) {
	s, client, _, _ := newS3Fixture(t, true, "")
	u, err := s.PresignPut(context.Background(), "x.pdf", time.Minute, "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, u, strings.NewReader("data"))
	req.Header.Set("Content-Type", "text/html")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mismatched content type: status %d", resp.StatusCode)
	}
}

func TestS3OpenErrors(t *testing.T) {
	none := func(string) string { return "" }
	for _, raw := range []string{
		"s3:///prefix",
		"s3://bkt/x?endpoint=ftp://h",
		"s3://bkt/x?path_style=maybe",
		"s3://bkt/../x",
	} {
		if _, err := Open(context.Background(), raw, Options{AWSAccessKeyID: "a", AWSSecretAccessKey: "b", Getenv: none}); err == nil {
			t.Errorf("Open(%q) succeeded", raw)
		}
	}
	if _, err := Open(context.Background(), "s3://bkt", Options{Getenv: none}); err == nil {
		t.Error("Open without credentials succeeded")
	}
	s, err := Open(context.Background(), "s3://bkt", Options{AWSAccessKeyID: "a", AWSSecretAccessKey: "b", Getenv: none})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PresignGet(context.Background(), "k", 8*24*time.Hour); err == nil {
		t.Error("ttl over 7 days accepted")
	}
	u, _ := s.PresignGet(context.Background(), "k", time.Hour)
	if !strings.HasPrefix(u, "https://bkt.s3.us-east-1.amazonaws.com/k?") {
		t.Errorf("default endpoint URL %s", u)
	}
}
