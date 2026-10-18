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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GCSStore talks to Google Cloud Storage through the JSON API. Requests are
// authorized with OAuth2 bearer tokens obtained from a service-account key
// (JWT bearer grant, RFC 7523) or from the GCE metadata server. Presigned URLs
// are V4 signed URLs (GOOG4-RSA-SHA256), signed locally with the
// service-account key or remotely with the IAM Credentials signBlob API.
type GCSStore struct {
	bucket   string
	prefix   string
	endpoint *url.URL // JSON API and signed-URL origin
	iamBase  string
	client   *http.Client
	now      func() time.Time
	tokens   tokenSource
	signer   blobSigner
}

const (
	gcsScope       = "https://www.googleapis.com/auth/devstorage.read_write"
	gcsIAMScope    = "https://www.googleapis.com/auth/cloud-platform"
	maxGCSPresign  = 7 * 24 * time.Hour
	gcsDefaultHost = "https://storage.googleapis.com"
)

type serviceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	TokenURI     string `json:"token_uri"`
}

func openGCS(ctx context.Context, u *url.URL, opts Options) (*GCSStore, error) {
	if u.Host == "" {
		return nil, fmt.Errorf("objectstore: gs URL needs a bucket: gs://bucket/prefix")
	}
	prefix, err := normPrefix(u.Path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	ep := q.Get("endpoint")
	if ep == "" {
		ep = gcsDefaultHost
	}
	epu, err := url.Parse(strings.TrimRight(ep, "/"))
	if err != nil || epu.Host == "" {
		return nil, fmt.Errorf("objectstore: gs endpoint %q must be http(s)://host", ep)
	}
	iam := q.Get("iam_endpoint")
	if iam == "" {
		iam = "https://iamcredentials.googleapis.com"
	}
	s := &GCSStore{
		bucket:   u.Host,
		prefix:   prefix,
		endpoint: epu,
		iamBase:  strings.TrimRight(iam, "/"),
		client:   opts.client(),
		now:      opts.now,
	}

	keyJSON := opts.GCSCredentialsJSON
	if len(keyJSON) == 0 {
		if p := opts.getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
			if keyJSON, err = os.ReadFile(p); err != nil {
				return nil, fmt.Errorf("objectstore: read GOOGLE_APPLICATION_CREDENTIALS: %w", err)
			}
		}
	}
	if len(keyJSON) > 0 {
		var sa serviceAccountKey
		if err := json.Unmarshal(keyJSON, &sa); err != nil {
			return nil, fmt.Errorf("objectstore: parse GCS credentials: %w", err)
		}
		if sa.Type != "service_account" {
			return nil, fmt.Errorf("objectstore: GCS credentials type %q not supported (want a service_account key)", sa.Type)
		}
		key, err := parseRSAPrivateKey(sa.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("objectstore: GCS service-account key: %w", err)
		}
		if sa.TokenURI == "" {
			sa.TokenURI = "https://oauth2.googleapis.com/token"
		}
		s.tokens = &cachedToken{fetch: (&jwtGrant{sa: sa, key: key, client: s.client, now: s.now}).fetch, now: s.now}
		s.signer = &rsaSigner{email: sa.ClientEmail, key: key}
		return s, nil
	}

	md := &metadataClient{host: opts.getenv("GCE_METADATA_HOST"), client: s.client, now: s.now}
	if md.host == "" {
		md.host = "metadata.google.internal"
	}
	s.tokens = &cachedToken{fetch: md.token, now: s.now}
	s.signer = &iamSigner{md: md, base: s.iamBase, tokens: s.tokens, client: s.client}
	return s, nil
}

func parseRSAPrivateKey(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("no PEM block in private_key")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private_key is not RSA")
		}
		return rk, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// --- tokens ---

type tokenSource interface {
	token(ctx context.Context) (string, error)
}

type cachedToken struct {
	fetch func(ctx context.Context) (string, time.Time, error)
	now   func() time.Time

	mu     sync.Mutex
	tok    string
	expiry time.Time
}

func (c *cachedToken) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" && c.now().Before(c.expiry.Add(-time.Minute)) {
		return c.tok, nil
	}
	tok, exp, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.tok, c.expiry = tok, exp
	return tok, nil
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

func decodeToken(resp *http.Response, now time.Time, what string) (string, time.Time, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", time.Time{}, fmt.Errorf("objectstore: %s: HTTP %d: %s", what, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", time.Time{}, fmt.Errorf("objectstore: %s: %w", what, err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("objectstore: %s: empty access_token", what)
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 300
	}
	return tr.AccessToken, now.Add(time.Duration(tr.ExpiresIn) * time.Second), nil
}

// jwtGrant implements the OAuth2 JWT bearer flow for service accounts.
type jwtGrant struct {
	sa     serviceAccountKey
	key    *rsa.PrivateKey
	client *http.Client
	now    func() time.Time
}

func (g *jwtGrant) assertion() (string, error) {
	now := g.now()
	hdr := map[string]string{"alg": "RS256", "typ": "JWT"}
	if g.sa.PrivateKeyID != "" {
		hdr["kid"] = g.sa.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   g.sa.ClientEmail,
		"scope": gcsScope + " " + gcsIAMScope,
		"aud":   g.sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	hb, _ := json.Marshal(hdr)
	cb, _ := json.Marshal(claims)
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (g *jwtGrant) fetch(ctx context.Context) (string, time.Time, error) {
	a, err := g.assertion()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("objectstore: sign JWT: %w", err)
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {a}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("objectstore: GCS token exchange: %w", err)
	}
	return decodeToken(resp, g.now(), "GCS token exchange")
}

// metadataClient reads tokens and the default service account from the GCE
// metadata server.
type metadataClient struct {
	host   string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	email string
}

func (m *metadataClient) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.host+"/computeMetadata/v1/"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	return m.client.Do(req)
}

func (m *metadataClient) token(ctx context.Context) (string, time.Time, error) {
	resp, err := m.get(ctx, "instance/service-accounts/default/token")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("objectstore: GCE metadata token: %w", err)
	}
	return decodeToken(resp, m.now(), "GCE metadata token")
}

func (m *metadataClient) serviceAccountEmail(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.email != "" {
		return m.email, nil
	}
	resp, err := m.get(ctx, "instance/service-accounts/default/email")
	if err != nil {
		return "", fmt.Errorf("objectstore: GCE metadata email: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("objectstore: GCE metadata email: HTTP %d", resp.StatusCode)
	}
	m.email = strings.TrimSpace(string(b))
	return m.email, nil
}

// --- signing ---

type blobSigner interface {
	account(ctx context.Context) (string, error)
	sign(ctx context.Context, data []byte) ([]byte, error)
}

type rsaSigner struct {
	email string
	key   *rsa.PrivateKey
}

func (r *rsaSigner) account(context.Context) (string, error) { return r.email, nil }
func (r *rsaSigner) sign(_ context.Context, data []byte) ([]byte, error) {
	sum := sha256.Sum256(data)
	return rsa.SignPKCS1v15(rand.Reader, r.key, crypto.SHA256, sum[:])
}

// iamSigner signs with the IAM Credentials signBlob API, for workloads that
// have no key file (GCE, GKE workload identity).
type iamSigner struct {
	md     *metadataClient
	base   string
	tokens tokenSource
	client *http.Client
}

func (s *iamSigner) account(ctx context.Context) (string, error) {
	return s.md.serviceAccountEmail(ctx)
}

func (s *iamSigner) sign(ctx context.Context, data []byte) ([]byte, error) {
	email, err := s.account(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := s.tokens.token(ctx)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(data)})
	u := s.base + "/v1/projects/-/serviceAccounts/" + url.PathEscape(email) + ":signBlob"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("objectstore: IAM signBlob: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("objectstore: IAM signBlob: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		SignedBlob string `json:"signedBlob"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("objectstore: IAM signBlob: %w", err)
	}
	return base64.StdEncoding.DecodeString(out.SignedBlob)
}

// --- object operations ---

func (s *GCSStore) apiURL(path string, rawPath string, q url.Values) string {
	u := *s.endpoint
	u.Path = path
	u.RawPath = rawPath
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (s *GCSStore) objectAPI(fullKey string, q url.Values) string {
	p := "/storage/v1/b/" + s.bucket + "/o/" + fullKey
	rp := "/storage/v1/b/" + url.PathEscape(s.bucket) + "/o/" + url.PathEscape(fullKey)
	return s.apiURL(p, rp, q)
}

func (s *GCSStore) do(ctx context.Context, method, u string, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	tok, err := s.tokens.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return s.client.Do(req)
}

type gcsObject struct {
	Name        string    `json:"name"`
	Size        string    `json:"size"`
	ContentType string    `json:"contentType"`
	Updated     time.Time `json:"updated"`
	ETag        string    `json:"etag"`
}

func (s *GCSStore) toInfo(o gcsObject) ObjectInfo {
	n, _ := strconv.ParseInt(o.Size, 10, 64)
	return ObjectInfo{Key: strings.TrimPrefix(o.Name, s.prefix), Size: n, ContentType: o.ContentType, LastModified: o.Updated, ETag: o.ETag}
}

// Put implements Store (simple media upload).
func (s *GCSStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	body, n, err := sizedBody(r, size)
	if err != nil {
		return err
	}
	if n == 0 {
		body = http.NoBody
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	q := url.Values{"uploadType": {"media"}, "name": {s.prefix + key}}
	u := s.apiURL("/upload/storage/v1/b/"+s.bucket+"/o", "/upload/storage/v1/b/"+url.PathEscape(s.bucket)+"/o", q)
	resp, err := s.do(ctx, http.MethodPost, u, body, n, http.Header{"Content-Type": {contentType}})
	if err != nil {
		return fmt.Errorf("objectstore: gcs put %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return httpError("gcs put", key, resp)
	}
	return nil
}

// Get implements Store.
func (s *GCSStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return nil, ObjectInfo{}, err
	}
	resp, err := s.do(ctx, http.MethodGet, s.objectAPI(s.prefix+key, url.Values{"alt": {"media"}}), nil, 0, nil)
	if err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("objectstore: gcs get %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, ObjectInfo{}, httpError("gcs get", key, resp)
	}
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if resp.ContentLength >= 0 {
		size = resp.ContentLength
	}
	lm, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	info := ObjectInfo{Key: key, Size: size, ContentType: resp.Header.Get("Content-Type"), LastModified: lm, ETag: resp.Header.Get("ETag")}
	return resp.Body, info, nil
}

// Stat implements Store.
func (s *GCSStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	resp, err := s.do(ctx, http.MethodGet, s.objectAPI(s.prefix+key, nil), nil, 0, nil)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstore: gcs stat %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ObjectInfo{}, httpError("gcs stat", key, resp)
	}
	var o gcsObject
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstore: gcs stat %q: %w", key, err)
	}
	return s.toInfo(o), nil
}

// Delete implements Store.
func (s *GCSStore) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, s.objectAPI(s.prefix+key, nil), nil, 0, nil)
	if err != nil {
		return fmt.Errorf("objectstore: gcs delete %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return httpError("gcs delete", key, resp)
	}
	return nil
}

// List implements Store.
func (s *GCSStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	var out []ObjectInfo
	page := ""
	for {
		q := url.Values{"prefix": {s.prefix + prefix}}
		if page != "" {
			q.Set("pageToken", page)
		}
		u := s.apiURL("/storage/v1/b/"+s.bucket+"/o", "/storage/v1/b/"+url.PathEscape(s.bucket)+"/o", q)
		resp, err := s.do(ctx, http.MethodGet, u, nil, 0, nil)
		if err != nil {
			return nil, fmt.Errorf("objectstore: gcs list %q: %w", prefix, err)
		}
		if resp.StatusCode != http.StatusOK {
			err := httpError("gcs list", prefix, resp)
			resp.Body.Close()
			return nil, err
		}
		var res struct {
			Items         []gcsObject `json:"items"`
			NextPageToken string      `json:"nextPageToken"`
		}
		err = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("objectstore: gcs list %q: %w", prefix, err)
		}
		for _, o := range res.Items {
			out = append(out, s.toInfo(o))
		}
		if res.NextPageToken == "" {
			break
		}
		page = res.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// PresignGet implements Store.
func (s *GCSStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return s.presign(ctx, http.MethodGet, key, ttl, "")
}

// PresignPut implements Store.
func (s *GCSStore) PresignPut(ctx context.Context, key string, ttl time.Duration, contentType string) (string, error) {
	return s.presign(ctx, http.MethodPut, key, ttl, contentType)
}

// presign builds a V4 signed URL:
// https://cloud.google.com/storage/docs/access-control/signing-urls-manually
func (s *GCSStore) presign(ctx context.Context, method, key string, ttl time.Duration, contentType string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if err := checkTTL(ttl, maxGCSPresign); err != nil {
		return "", err
	}
	email, err := s.signer.account(ctx)
	if err != nil {
		return "", err
	}
	t := s.now().UTC()
	datetime := t.Format(amzDateFormat)
	scope := t.Format("20060102") + "/auto/storage/goog4_request"
	host := s.endpoint.Host
	signedHeaders := "host"
	headers := "host:" + host + "\n"
	if contentType != "" {
		signedHeaders = "content-type;host"
		headers = "content-type:" + trimHeaderValue(contentType) + "\n" + headers
	}
	q := url.Values{}
	q.Set("X-Goog-Algorithm", "GOOG4-RSA-SHA256")
	q.Set("X-Goog-Credential", email+"/"+scope)
	q.Set("X-Goog-Date", datetime)
	q.Set("X-Goog-Expires", formatSeconds(ttl))
	q.Set("X-Goog-SignedHeaders", signedHeaders)
	path := "/" + s.bucket + "/" + s.prefix + key
	encPath := awsURIEncode(path, true)
	creq := strings.Join([]string{method, encPath, canonicalQuery(q), headers, signedHeaders, unsignedPayload}, "\n")
	sts := "GOOG4-RSA-SHA256\n" + datetime + "\n" + scope + "\n" + sha256Hex([]byte(creq))
	sig, err := s.signer.sign(ctx, []byte(sts))
	if err != nil {
		return "", err
	}
	q.Set("X-Goog-Signature", hex.EncodeToString(sig))
	return s.endpoint.Scheme + "://" + host + encPath + "?" + canonicalQuery(q), nil
}
