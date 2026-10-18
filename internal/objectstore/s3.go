package objectstore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Store talks to Amazon S3 or an S3-compatible server (MinIO, Ceph RGW,
// R2) with hand-rolled SigV4.
type S3Store struct {
	bucket    string
	prefix    string
	scheme    string // http or https
	host      string // endpoint host[:port]
	pathStyle bool
	region    string
	creds     awsCredSource
	client    *http.Client
	now       func() time.Time
}

// maxS3Presign is the SigV4 query-auth limit (7 days).
const maxS3Presign = 7 * 24 * time.Hour

func openS3(u *url.URL, opts Options) (*S3Store, error) {
	bucket := u.Host
	if bucket == "" {
		return nil, fmt.Errorf("objectstore: s3 URL needs a bucket: s3://bucket/prefix")
	}
	prefix, err := normPrefix(u.Path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	region := q.Get("region")
	if region == "" {
		region = opts.getenv("AWS_REGION")
	}
	if region == "" {
		region = opts.getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "us-east-1"
	}
	caClient, err := s3HTTPClient(opts)
	if err != nil {
		return nil, err
	}
	client := opts.client()
	if caClient != nil {
		client = caClient
	}
	creds, err := s3CredSource(opts, region, caClient)
	if err != nil {
		return nil, err
	}
	s := &S3Store{
		bucket: bucket,
		prefix: prefix,
		scheme: "https",
		host:   "s3." + region + ".amazonaws.com",
		region: region,
		creds:  creds,
		client: client,
		now:    opts.now,
	}
	if ep := q.Get("endpoint"); ep != "" {
		eu, err := url.Parse(ep)
		if err != nil || eu.Host == "" || (eu.Scheme != "http" && eu.Scheme != "https") {
			return nil, fmt.Errorf("objectstore: s3 endpoint %q must be http(s)://host[:port]", ep)
		}
		s.scheme, s.host = eu.Scheme, eu.Host
	}
	if ps := q.Get("path_style"); ps != "" {
		s.pathStyle, err = strconv.ParseBool(ps)
		if err != nil {
			return nil, fmt.Errorf("objectstore: s3 path_style=%q: %w", ps, err)
		}
	}
	// Bucket names with dots break virtual-hosted TLS certificates.
	if strings.Contains(bucket, ".") && s.scheme == "https" {
		s.pathStyle = true
	}
	return s, nil
}

// objectURL returns the URL for a full object key (prefix included) or, for
// key == "", the bucket root.
func (s *S3Store) objectURL(fullKey string, q url.Values) *url.URL {
	u := &url.URL{Scheme: s.scheme, Host: s.host}
	p := "/" + fullKey
	if s.pathStyle {
		p = "/" + s.bucket + p
	} else {
		u.Host = s.bucket + "." + s.host
	}
	u.Path = p
	u.RawPath = awsURIEncode(p, true)
	if q != nil {
		u.RawQuery = canonicalQuery(q)
	}
	return u
}

func (s *S3Store) do(ctx context.Context, method string, u *url.URL, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	payload := emptyPayloadHash
	if body != nil {
		req.ContentLength = size
		payload = unsignedPayload
	}
	signer, err := s.signer(ctx, 0)
	if err != nil {
		return nil, err
	}
	signer.Sign(req, payload, s.now())
	return s.client.Do(req)
}

// signer returns a SigV4 signer with credentials that stay valid for at
// least minValid (the lifetime of a presigned URL).
func (s *S3Store) signer(ctx context.Context, minValid time.Duration) (sigV4, error) {
	creds, err := s.creds.Retrieve(ctx, minValid)
	if err != nil {
		return sigV4{}, err
	}
	return sigV4{creds: creds, region: s.region, service: "s3"}, nil
}

// s3HTTPClient returns a client that also trusts the certificates in
// AWS_CA_BUNDLE, as the AWS SDKs do, for an S3-compatible endpoint or STS
// behind a private CA. It returns nil when Options.HTTPClient is set or
// AWS_CA_BUNDLE is empty.
func s3HTTPClient(opts Options) (*http.Client, error) {
	path := opts.getenv("AWS_CA_BUNDLE")
	if opts.HTTPClient != nil || path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("objectstore: read AWS_CA_BUNDLE: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("objectstore: AWS_CA_BUNDLE %s has no PEM certificate", path)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr}, nil
}

// s3CredSource picks the credential source. See awscreds.go for the order.
// caClient, when not nil, is used for STS too (AWS_CA_BUNDLE).
func s3CredSource(opts Options, region string, caClient *http.Client) (awsCredSource, error) {
	creds := awsCreds{opts.AWSAccessKeyID, opts.AWSSecretAccessKey, opts.AWSSessionToken}
	if creds.AccessKeyID == "" {
		creds = awsCreds{opts.getenv("AWS_ACCESS_KEY_ID"), opts.getenv("AWS_SECRET_ACCESS_KEY"), opts.getenv("AWS_SESSION_TOKEN")}
	}
	if creds.AccessKeyID != "" || creds.SecretAccessKey != "" {
		if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
			return nil, fmt.Errorf("objectstore: s3 credentials incomplete: set both AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
		}
		return staticAWSCreds(creds), nil
	}
	stsOpts := opts
	if stsOpts.HTTPClient == nil && caClient != nil {
		stsOpts.HTTPClient = caClient
	}
	wi, err := newWebIdentityAWSCreds(stsOpts, region, os.ReadFile)
	if err != nil {
		return nil, err
	}
	if wi != nil {
		return wi, nil
	}
	cc, err := newContainerAWSCreds(opts, os.ReadFile)
	if err != nil {
		return nil, err
	}
	if cc != nil {
		return cc, nil
	}
	return nil, fmt.Errorf("objectstore: s3 credentials missing: set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or run with EKS Pod Identity, IRSA (%s) or an ECS task role (%s)", envWebIdentityToken, envContainerFullURI)
}

// Put implements Store.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	body, n, err := sizedBody(r, size)
	if err != nil {
		return err
	}
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	if n == 0 {
		body = http.NoBody
	}
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(s.prefix+key, nil), body, n, h)
	if err != nil {
		return fmt.Errorf("objectstore: s3 put %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return httpError("s3 put", key, resp)
	}
	return nil
}

func (s *S3Store) infoFromHeaders(key string, h http.Header) ObjectInfo {
	size, _ := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	lm, _ := http.ParseTime(h.Get("Last-Modified"))
	return ObjectInfo{Key: key, Size: size, ContentType: h.Get("Content-Type"), LastModified: lm, ETag: strings.Trim(h.Get("ETag"), `"`)}
}

// Get implements Store.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return nil, ObjectInfo{}, err
	}
	resp, err := s.do(ctx, http.MethodGet, s.objectURL(s.prefix+key, nil), nil, 0, nil)
	if err != nil {
		return nil, ObjectInfo{}, fmt.Errorf("objectstore: s3 get %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, ObjectInfo{}, httpError("s3 get", key, resp)
	}
	return resp.Body, s.infoFromHeaders(key, resp.Header), nil
}

// Stat implements Store.
func (s *S3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	resp, err := s.do(ctx, http.MethodHead, s.objectURL(s.prefix+key, nil), nil, 0, nil)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("objectstore: s3 stat %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ObjectInfo{}, httpError("s3 stat", key, resp)
	}
	return s.infoFromHeaders(key, resp.Header), nil
}

// Delete implements Store.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, s.objectURL(s.prefix+key, nil), nil, 0, nil)
	if err != nil {
		return fmt.Errorf("objectstore: s3 delete %q: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return httpError("s3 delete", key, resp)
	}
	return nil
}

type s3ListResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
		ETag         string    `xml:"ETag"`
	} `xml:"Contents"`
}

// List implements Store (ListObjectsV2 with pagination). ContentType is not
// returned by S3 listings and is left empty.
func (s *S3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	var out []ObjectInfo
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {s.prefix + prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, s.objectURL("", q), nil, 0, nil)
		if err != nil {
			return nil, fmt.Errorf("objectstore: s3 list %q: %w", prefix, err)
		}
		if resp.StatusCode != http.StatusOK {
			err := httpError("s3 list", prefix, resp)
			resp.Body.Close()
			return nil, err
		}
		var res s3ListResult
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("objectstore: s3 list %q: decode: %w", prefix, err)
		}
		for _, c := range res.Contents {
			out = append(out, ObjectInfo{
				Key:          strings.TrimPrefix(c.Key, s.prefix),
				Size:         c.Size,
				LastModified: c.LastModified,
				ETag:         strings.Trim(c.ETag, `"`),
			})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		token = res.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// PresignGet implements Store.
func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if err := checkTTL(ttl, maxS3Presign); err != nil {
		return "", err
	}
	signer, err := s.signer(ctx, ttl)
	if err != nil {
		return "", err
	}
	return signer.Presign(http.MethodGet, s.objectURL(s.prefix+key, nil), nil, s.now(), ttl), nil
}

// PresignPut implements Store.
func (s *S3Store) PresignPut(ctx context.Context, key string, ttl time.Duration, contentType string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if err := checkTTL(ttl, maxS3Presign); err != nil {
		return "", err
	}
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	signer, err := s.signer(ctx, ttl)
	if err != nil {
		return "", err
	}
	return signer.Presign(http.MethodPut, s.objectURL(s.prefix+key, nil), h, s.now(), ttl), nil
}
