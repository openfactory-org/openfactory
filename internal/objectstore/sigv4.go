package objectstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4, implemented from the public specification:
// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html

const (
	sigV4Algorithm   = "AWS4-HMAC-SHA256"
	amzDateFormat    = "20060102T150405Z"
	unsignedPayload  = "UNSIGNED-PAYLOAD"
	emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

type awsCreds struct {
	AccessKeyID, SecretAccessKey, SessionToken string
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// awsURIEncode encodes s per the SigV4 rules: unreserved characters
// (A-Z a-z 0-9 - _ . ~) are kept, everything else is %XX (uppercase). When
// keepSlash is set, "/" is kept.
func awsURIEncode(s string, keepSlash bool) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexd[c>>4])
			b.WriteByte(hexd[c&15])
		}
	}
	return b.String()
}

// canonicalQuery sorts and encodes query parameters.
func canonicalQuery(q url.Values) string {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{awsURIEncode(k, false), awsURIEncode(v, false)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// signedHeaderNames picks the headers to sign: host, content-type,
// content-md5, range and every x-amz-* header.
func signedHeaderNames(h http.Header) []string {
	names := []string{"host"}
	for k := range h {
		lk := strings.ToLower(k)
		switch {
		case lk == "host":
		case lk == "content-type", lk == "content-md5", lk == "range", strings.HasPrefix(lk, "x-amz-"):
			names = append(names, lk)
		}
	}
	sort.Strings(names)
	return names
}

func trimHeaderValue(v string) string { return strings.Join(strings.Fields(v), " ") }

// canonicalHeaders renders "name:value\n" lines for names (sorted, lowercase).
func canonicalHeaders(names []string, host string, h http.Header) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		if n == "host" {
			b.WriteString(host)
		} else {
			vals := h.Values(n)
			for i, v := range vals {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(trimHeaderValue(v))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

type sigV4 struct {
	creds   awsCreds
	region  string
	service string
}

func (s sigV4) scope(t time.Time) string {
	return t.Format("20060102") + "/" + s.region + "/" + s.service + "/aws4_request"
}

func (s sigV4) signingKey(t time.Time) []byte {
	k := hmacSHA256([]byte("AWS4"+s.creds.SecretAccessKey), t.Format("20060102"))
	k = hmacSHA256(k, s.region)
	k = hmacSHA256(k, s.service)
	return hmacSHA256(k, "aws4_request")
}

func (s sigV4) signature(t time.Time, canonicalRequest string) string {
	sts := sigV4Algorithm + "\n" + t.Format(amzDateFormat) + "\n" + s.scope(t) + "\n" + sha256Hex([]byte(canonicalRequest))
	return hex.EncodeToString(hmacSHA256(s.signingKey(t), sts))
}

// canonicalURI returns the encoded path. path must be the decoded path.
// S3 encodes once; other services encode the already-encoded path again.
func (s sigV4) canonicalURI(path string) string {
	if path == "" {
		path = "/"
	}
	enc := awsURIEncode(path, true)
	if s.service != "s3" {
		enc = awsURIEncode(enc, true)
	}
	return enc
}

func requestHost(r *http.Request) string {
	if r.Host != "" {
		return r.Host
	}
	return r.URL.Host
}

// Sign adds X-Amz-Date (and X-Amz-Security-Token) and the Authorization
// header to r. payloadHash is the hex SHA-256 of the body or
// UNSIGNED-PAYLOAD; for S3 it is also sent as X-Amz-Content-Sha256.
func (s sigV4) Sign(r *http.Request, payloadHash string, t time.Time) {
	t = t.UTC()
	r.Header.Set("X-Amz-Date", t.Format(amzDateFormat))
	if s.creds.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", s.creds.SessionToken)
	}
	if s.service == "s3" {
		r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	host := requestHost(r)
	names := signedHeaderNames(r.Header)
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{
		r.Method,
		s.canonicalURI(r.URL.Path),
		canonicalQuery(r.URL.Query()),
		canonicalHeaders(names, host, r.Header),
		signed,
		payloadHash,
	}, "\n")
	r.Header.Set("Authorization", sigV4Algorithm+" Credential="+s.creds.AccessKeyID+"/"+s.scope(t)+
		", SignedHeaders="+signed+", Signature="+s.signature(t, creq))
}

// Presign returns u with query-string authentication added. extra holds
// headers the client must send (they are signed; host is always signed).
func (s sigV4) Presign(method string, u *url.URL, extra http.Header, t time.Time, ttl time.Duration) string {
	t = t.UTC()
	q := u.Query()
	q.Set("X-Amz-Algorithm", sigV4Algorithm)
	q.Set("X-Amz-Credential", s.creds.AccessKeyID+"/"+s.scope(t))
	q.Set("X-Amz-Date", t.Format(amzDateFormat))
	q.Set("X-Amz-Expires", formatSeconds(ttl))
	if s.creds.SessionToken != "" {
		q.Set("X-Amz-Security-Token", s.creds.SessionToken)
	}
	if extra == nil {
		extra = http.Header{}
	}
	names := signedHeaderNames(extra)
	signed := strings.Join(names, ";")
	q.Set("X-Amz-SignedHeaders", signed)
	creq := strings.Join([]string{
		method,
		s.canonicalURI(u.Path),
		canonicalQuery(q),
		canonicalHeaders(names, u.Host, extra),
		signed,
		unsignedPayload,
	}, "\n")
	q.Set("X-Amz-Signature", s.signature(t, creq))
	out := *u
	out.RawQuery = canonicalQuery(q)
	return out.String()
}

func formatSeconds(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10)
}
