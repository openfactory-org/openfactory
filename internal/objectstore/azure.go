package objectstore

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AzureStore talks to Azure Blob Storage with Shared Key authorization and
// issues Service SAS URLs for presigned access.
type AzureStore struct {
	account   string
	container string
	prefix    string
	base      *url.URL // https://account.blob.core.windows.net or an emulator base incl. account path
	key       []byte
	client    *http.Client
	now       func() time.Time
}

const (
	azureVersion    = "2021-08-06"
	azureTimeFormat = "2006-01-02T15:04:05Z"
)

func openAzure(u *url.URL, opts Options) (*AzureStore, error) {
	account := u.Host
	parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 2)
	if account == "" || parts[0] == "" {
		return nil, fmt.Errorf("objectstore: azblob URL must be azblob://account/container/prefix")
	}
	prefix := ""
	if len(parts) == 2 {
		var err error
		if prefix, err = normPrefix(parts[1]); err != nil {
			return nil, err
		}
	}
	keyB64 := opts.AzureAccountKey
	if keyB64 == "" {
		keyB64 = opts.getenv("AZURE_STORAGE_KEY")
	}
	if keyB64 == "" {
		keyB64 = opts.getenv("AZURE_STORAGE_ACCOUNT_KEY")
	}
	if keyB64 == "" {
		return nil, fmt.Errorf("objectstore: azure account key missing: set AZURE_STORAGE_KEY")
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("objectstore: azure account key is not base64: %w", err)
	}
	ep := u.Query().Get("endpoint")
	if ep == "" {
		ep = "https://" + account + ".blob.core.windows.net"
	}
	base, err := url.Parse(strings.TrimRight(ep, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("objectstore: azblob endpoint %q must be http(s)://host[/account]", ep)
	}
	return &AzureStore{
		account:   account,
		container: parts[0],
		prefix:    prefix,
		base:      base,
		key:       key,
		client:    opts.client(),
		now:       opts.now,
	}, nil
}

// blobURL returns the URL of a blob (fullKey != "") or of the container.
func (s *AzureStore) blobURL(fullKey string, q url.Values) *url.URL {
	u := *s.base
	p := s.base.Path + "/" + s.container
	if fullKey != "" {
		p += "/" + fullKey
	}
	u.Path = p
	u.RawPath = "/" + escapePath(strings.TrimPrefix(p, "/"))
	if q != nil {
		u.RawQuery = q.Encode()
	}
	return &u
}

// sharedKeyString builds the Shared Key string-to-sign:
// https://learn.microsoft.com/rest/api/storageservices/authorize-with-shared-key
func (s *AzureStore) sharedKeyString(r *http.Request) string {
	h := r.Header
	cl := ""
	if r.ContentLength > 0 {
		cl = strconv.FormatInt(r.ContentLength, 10)
	}
	var b strings.Builder
	b.WriteString(r.Method + "\n")
	for _, v := range []string{
		h.Get("Content-Encoding"), h.Get("Content-Language"), cl, h.Get("Content-MD5"),
		h.Get("Content-Type"), h.Get("Date"), h.Get("If-Modified-Since"), h.Get("If-Match"),
		h.Get("If-None-Match"), h.Get("If-Unmodified-Since"), h.Get("Range"),
	} {
		b.WriteString(v + "\n")
	}
	var names []string
	for k := range h {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-ms-") {
			names = append(names, lk)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString(n + ":" + trimHeaderValue(strings.Join(h.Values(n), ",")) + "\n")
	}
	b.WriteString("/" + s.account + r.URL.EscapedPath())
	q := r.URL.Query()
	var qn []string
	for k := range q {
		qn = append(qn, k)
	}
	sort.Slice(qn, func(i, j int) bool { return strings.ToLower(qn[i]) < strings.ToLower(qn[j]) })
	for _, k := range qn {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		b.WriteString("\n" + strings.ToLower(k) + ":" + strings.Join(vals, ","))
	}
	return b.String()
}

func (s *AzureStore) do(ctx context.Context, method string, u *url.URL, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.ContentLength = size
	}
	req.Header.Set("x-ms-date", s.now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", azureVersion)
	sig := base64.StdEncoding.EncodeToString(hmacSHA256(s.key, s.sharedKeyString(req)))
	req.Header.Set("Authorization", "SharedKey "+s.account+":"+sig)
	return s.client.Do(req)
}

func (s *AzureStore) infoFromHeaders(key string, h http.Header) ObjectInfo {
	size, _ := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	lm, _ := http.ParseTime(h.Get("Last-Modified"))
	return ObjectInfo{Key: key, Size: size, ContentType: h.Get("Content-Type"), LastModified: lm, ETag: strings.Trim(h.Get("ETag"), `"`)}
}

// Put implements Store (single Put Blob, block blob).
func (s *AzureStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
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
	h := http.Header{"X-Ms-Blob-Type": {"BlockBlob"}}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	resp, err := s.do(ctx, http.MethodPut, s.blobURL(s.prefix+key, nil), body, n, h)
	if err != nil {
		return fmt.Errorf("objectstore: azure put %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return httpError("azure put", key, resp)
	}
	return nil
}

// Get implements Store.
func (s *AzureStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return nil, ObjectInfo{}, err
	}
	resp, err := s.do(ctx, http.MethodGet, s.blobURL(s.prefix+key, nil), nil, 0, nil)
	if err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("objectstore: azure get %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, ObjectInfo{}, httpError("azure get", key, resp)
	}
	return resp.Body, s.infoFromHeaders(key, resp.Header), nil
}

// Stat implements Store.
func (s *AzureStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	resp, err := s.do(ctx, http.MethodHead, s.blobURL(s.prefix+key, nil), nil, 0, nil)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstore: azure stat %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ObjectInfo{}, httpError("azure stat", key, resp)
	}
	return s.infoFromHeaders(key, resp.Header), nil
}

// Delete implements Store.
func (s *AzureStore) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, s.blobURL(s.prefix+key, nil), nil, 0, nil)
	if err != nil {
		return fmt.Errorf("objectstore: azure delete %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return httpError("azure delete", key, resp)
	}
	return nil
}

type azureList struct {
	Blobs []struct {
		Name       string `xml:"Name"`
		Properties struct {
			LastModified  string `xml:"Last-Modified"`
			Etag          string `xml:"Etag"`
			ContentLength int64  `xml:"Content-Length"`
			ContentType   string `xml:"Content-Type"`
		} `xml:"Properties"`
	} `xml:"Blobs>Blob"`
	NextMarker string `xml:"NextMarker"`
}

// List implements Store (List Blobs with marker pagination).
func (s *AzureStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	var out []ObjectInfo
	marker := ""
	for {
		q := url.Values{"restype": {"container"}, "comp": {"list"}, "prefix": {s.prefix + prefix}}
		if marker != "" {
			q.Set("marker", marker)
		}
		resp, err := s.do(ctx, http.MethodGet, s.blobURL("", q), nil, 0, nil)
		if err != nil {
			return nil, fmt.Errorf("objectstore: azure list %q: %w", prefix, err)
		}
		if resp.StatusCode != http.StatusOK {
			err := httpError("azure list", prefix, resp)
			resp.Body.Close()
			return nil, err
		}
		var res azureList
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("objectstore: azure list %q: decode: %w", prefix, err)
		}
		for _, b := range res.Blobs {
			lm, _ := http.ParseTime(b.Properties.LastModified)
			out = append(out, ObjectInfo{
				Key:          strings.TrimPrefix(b.Name, s.prefix),
				Size:         b.Properties.ContentLength,
				ContentType:  b.Properties.ContentType,
				LastModified: lm,
				ETag:         strings.Trim(b.Properties.Etag, `"`),
			})
		}
		if res.NextMarker == "" {
			break
		}
		marker = res.NextMarker
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// PresignGet implements Store with a read-only Service SAS.
func (s *AzureStore) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	return s.sas(key, "r", ttl)
}

// PresignPut implements Store with a create/write Service SAS. Azure cannot
// bind the Content-Type into a SAS; clients must send the headers returned
// by PutHeaders, including "x-ms-blob-type: BlockBlob".
func (s *AzureStore) PresignPut(_ context.Context, key string, ttl time.Duration, _ string) (string, error) {
	return s.sas(key, "cw", ttl)
}

func (s *AzureStore) presignPutHeaders(contentType string) http.Header {
	h := http.Header{"X-Ms-Blob-Type": {"BlockBlob"}}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return h
}

// sasStringToSign builds the Service SAS string-to-sign for version
// 2020-12-06 and later:
// https://learn.microsoft.com/rest/api/storageservices/create-service-sas
func sasStringToSign(perms, start, expiry, canonicalResource, version, resource string) string {
	return strings.Join([]string{
		perms, start, expiry, canonicalResource,
		"", // signedIdentifier
		"", // signedIP
		"", // signedProtocol
		version, resource,
		"",                 // signedSnapshotTime
		"",                 // signedEncryptionScope
		"", "", "", "", "", // rscc, rscd, rsce, rscl, rsct
	}, "\n")
}

func (s *AzureStore) sas(key, perms string, ttl time.Duration) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if err := checkTTL(ttl, 0); err != nil {
		return "", err
	}
	now := s.now().UTC()
	start := now.Add(-5 * time.Minute).Format(azureTimeFormat) // tolerate clock skew
	expiry := now.Add(ttl).Format(azureTimeFormat)
	full := s.prefix + key
	cr := "/blob/" + s.account + "/" + s.container + "/" + full
	sig := base64.StdEncoding.EncodeToString(hmacSHA256(s.key, sasStringToSign(perms, start, expiry, cr, azureVersion, "b")))
	q := url.Values{
		"sv":  {azureVersion},
		"sp":  {perms},
		"sr":  {"b"},
		"st":  {start},
		"se":  {expiry},
		"sig": {sig},
	}
	return s.blobURL(full, q).String(), nil
}
