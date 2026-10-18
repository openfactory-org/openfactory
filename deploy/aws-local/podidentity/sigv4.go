package main

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

// A small AWS Signature Version 4 signer for non-S3 services (header signing
// and query presigning), from the public specification:
// https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html

const (
	sigAlgorithm  = "AWS4-HMAC-SHA256"
	amzDateFormat = "20060102T150405Z"
	// emptyPayload is the hex SHA-256 of an empty body.
	emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

type awsKeys struct{ AccessKeyID, SecretAccessKey, SessionToken string }

type signer struct {
	keys    awsKeys
	region  string
	service string
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

// uriEncode keeps A-Z a-z 0-9 - _ . ~ (and "/" when keepSlash) and encodes the rest.
func uriEncode(s string, keepSlash bool) string {
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

func canonicalQuery(q url.Values) string {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{uriEncode(k, false), uriEncode(v, false)})
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

// signedNames picks the headers to sign: host, content-type, content-md5 and
// every x-amz-* header, plus the extra names (for example x-k8s-aws-id).
func signedNames(h http.Header, extra ...string) []string {
	names := map[string]bool{"host": true}
	for k := range h {
		lk := strings.ToLower(k)
		if lk == "content-type" || lk == "content-md5" || strings.HasPrefix(lk, "x-amz-") {
			names[lk] = true
		}
	}
	for _, e := range extra {
		names[strings.ToLower(e)] = true
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func canonicalHeaders(names []string, host string, h http.Header) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n + ":")
		if n == "host" {
			b.WriteString(host)
		} else {
			for i, v := range h.Values(n) {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(strings.Join(strings.Fields(v), " "))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (s signer) scope(t time.Time) string {
	return t.Format("20060102") + "/" + s.region + "/" + s.service + "/aws4_request"
}

func (s signer) signature(t time.Time, creq string) string {
	k := hmacSHA256([]byte("AWS4"+s.keys.SecretAccessKey), t.Format("20060102"))
	k = hmacSHA256(k, s.region)
	k = hmacSHA256(k, s.service)
	k = hmacSHA256(k, "aws4_request")
	sts := sigAlgorithm + "\n" + t.Format(amzDateFormat) + "\n" + s.scope(t) + "\n" + sha256Hex([]byte(creq))
	return hex.EncodeToString(hmacSHA256(k, sts))
}

// canonicalURI encodes the path twice, as every service but S3 does.
func canonicalURI(path string) string {
	if path == "" {
		path = "/"
	}
	return uriEncode(uriEncode(path, true), true)
}

// Sign adds X-Amz-Date, X-Amz-Security-Token (when set) and Authorization to r.
// payloadHash is the hex SHA-256 of the body.
func (s signer) Sign(r *http.Request, payloadHash string, t time.Time) {
	t = t.UTC()
	r.Header.Set("X-Amz-Date", t.Format(amzDateFormat))
	if s.keys.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", s.keys.SessionToken)
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	names := signedNames(r.Header)
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{
		r.Method, canonicalURI(r.URL.Path), canonicalQuery(r.URL.Query()),
		canonicalHeaders(names, host, r.Header), signed, payloadHash,
	}, "\n")
	r.Header.Set("Authorization", sigAlgorithm+" Credential="+s.keys.AccessKeyID+"/"+s.scope(t)+
		", SignedHeaders="+signed+", Signature="+s.signature(t, creq))
}

// Presign returns u with query-string authentication. extra holds headers
// that the caller sends and that are signed (host is always signed). A query
// signature of a non-S3 service covers the hash of the empty body.
func (s signer) Presign(method string, u *url.URL, extra http.Header, t time.Time, ttl time.Duration) string {
	t = t.UTC()
	q := u.Query()
	q.Set("X-Amz-Algorithm", sigAlgorithm)
	q.Set("X-Amz-Credential", s.keys.AccessKeyID+"/"+s.scope(t))
	q.Set("X-Amz-Date", t.Format(amzDateFormat))
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(ttl/time.Second), 10))
	if s.keys.SessionToken != "" {
		q.Set("X-Amz-Security-Token", s.keys.SessionToken)
	}
	names := []string{"host"}
	for k := range extra {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	signed := strings.Join(names, ";")
	q.Set("X-Amz-SignedHeaders", signed)
	creq := strings.Join([]string{
		method, canonicalURI(u.Path), canonicalQuery(q),
		canonicalHeaders(names, u.Host, extra), signed, emptyPayload,
	}, "\n")
	q.Set("X-Amz-Signature", s.signature(t, creq))
	out := *u
	out.RawQuery = canonicalQuery(q)
	return out.String()
}
