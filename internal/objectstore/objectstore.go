// Package objectstore stores run data (transcripts, artifacts and prompt
// attachments) in pluggable object storage: local disk, Amazon S3 (and
// S3-compatible servers such as MinIO), Google Cloud Storage and Azure Blob
// Storage.
//
// All drivers are implemented against the providers' public REST protocols
// with the standard library only. Nothing is cached: every read goes to the
// backing store, and clients are expected to fetch large objects through
// presigned URLs so that data flows directly between the bucket and the
// client.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrNotFound is returned (possibly wrapped) when an object does not exist.
var ErrNotFound = errors.New("objectstore: object not found")

// ErrInvalidKey is returned (possibly wrapped) for keys that are empty,
// absolute, or that contain "." / ".." segments, backslashes or control
// characters.
var ErrInvalidKey = errors.New("objectstore: invalid key")

// ErrPresignUnsupported is returned when a store cannot produce presigned URLs
// with its current configuration (for example a local store without a signing
// key or base URL).
var ErrPresignUnsupported = errors.New("objectstore: presigned URLs not available")

// ObjectInfo describes a stored object. Key is relative to the store's
// configured prefix.
type ObjectInfo struct {
	Key          string
	Size         int64
	ContentType  string
	LastModified time.Time
	ETag         string
}

// Store is a flat key/value object store. Keys use "/" as a separator and are
// validated by every driver (see ErrInvalidKey).
type Store interface {
	// Put writes an object, replacing any existing one. size is the exact
	// body length, or -1 when unknown (the body is then buffered in memory
	// for drivers that need a length).
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get opens an object for reading. The caller must close the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	// Stat returns an object's metadata.
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// List returns every object whose key starts with prefix, sorted by key.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// PresignGet returns a URL that allows an unauthenticated GET of the
	// object until ttl elapses.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// PresignPut returns a URL that allows an unauthenticated PUT of the
	// object until ttl elapses. When contentType is not empty the client
	// must send exactly that Content-Type. Use PutHeaders for the full set
	// of headers a client must send.
	PresignPut(ctx context.Context, key string, ttl time.Duration, contentType string) (string, error)
}

// putHeaderer is implemented by drivers that need extra headers on a
// presigned PUT.
type putHeaderer interface {
	presignPutHeaders(contentType string) http.Header
}

// PutHeaders returns the headers a client must send with a PUT to a URL
// produced by s.PresignPut(…, contentType). Azure Blob Storage, for example,
// requires "x-ms-blob-type: BlockBlob".
func PutHeaders(s Store, contentType string) http.Header {
	if p, ok := s.(putHeaderer); ok {
		return p.presignPutHeaders(contentType)
	}
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return h
}

// Options configure Open. Zero values select sensible defaults; credentials
// not set here are read from the environment.
type Options struct {
	// HTTPClient is used for all remote calls. Default: a client with a
	// 60s timeout for metadata/token calls and no timeout for transfers.
	HTTPClient *http.Client
	// Now returns the current time (tests). Default time.Now.
	Now func() time.Time
	// Getenv reads environment variables (tests). Default os.Getenv.
	Getenv func(string) string

	// Local disk (file://).

	// SigningKey is the HMAC key for local presigned URLs. Required for
	// PresignGet/PresignPut on a file:// store; use the same key on every
	// replica.
	SigningKey []byte
	// BaseURL is the externally reachable server origin (for example
	// "https://factory.example.com") that local presigned URLs point at.
	BaseURL string
	// RoutePrefix is the URL path the local Handler is mounted under.
	// Default "/objects/".
	RoutePrefix string
	// MaxPutBytes caps bodies accepted by the local Handler. Default 5 GiB.
	MaxPutBytes int64

	// Amazon S3 (s3://). Default: AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
	// AWS_SESSION_TOKEN, and AWS_REGION / AWS_DEFAULT_REGION.
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AWSSessionToken    string

	// Google Cloud Storage (gs://).

	// GCSCredentialsJSON is a service-account JSON key. Default: the file
	// named by GOOGLE_APPLICATION_CREDENTIALS, else the GCE metadata server.
	GCSCredentialsJSON []byte

	// Azure Blob Storage (azblob://). Default: AZURE_STORAGE_KEY.
	AzureAccountKey string
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) getenv(k string) string {
	if o.Getenv != nil {
		return o.Getenv(k)
	}
	return os.Getenv(k)
}

func (o Options) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return http.DefaultClient
}

// Open returns a Store for url. Supported forms:
//
//	file:///abs/path
//	s3://bucket/prefix?region=us-east-1&endpoint=http://minio:9000&path_style=true
//	gs://bucket/prefix?endpoint=https://storage.googleapis.com
//	azblob://account/container/prefix?endpoint=http://127.0.0.1:10000/devstoreaccount1
//
// The optional prefix is prepended to every key.
func Open(ctx context.Context, rawURL string, opts Options) (Store, error) {
	scheme, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		return nil, fmt.Errorf("objectstore: %q is not a URL (want file://, s3://, gs:// or azblob://)", rawURL)
	}
	switch strings.ToLower(scheme) {
	case "file":
		return openFile(rest, opts)
	case "s3":
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("objectstore: parse %q: %w", rawURL, err)
		}
		return openS3(u, opts)
	case "gs":
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("objectstore: parse %q: %w", rawURL, err)
		}
		return openGCS(ctx, u, opts)
	case "azblob":
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("objectstore: parse %q: %w", rawURL, err)
		}
		return openAzure(u, opts)
	default:
		return nil, fmt.Errorf("objectstore: unsupported scheme %q (want file, s3, gs or azblob)", scheme)
	}
}

// ValidateKey reports whether key is acceptable to every driver.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty", ErrInvalidKey)
	}
	if len(key) > 1024 {
		return fmt.Errorf("%w: longer than 1024 bytes", ErrInvalidKey)
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("%w: %q has a leading or trailing slash", ErrInvalidKey, key)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("%w: %q contains a control character or backslash", ErrInvalidKey, key)
		}
	}
	for _, seg := range strings.Split(key, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%w: %q has an empty segment", ErrInvalidKey, key)
		case ".", "..":
			return fmt.Errorf("%w: %q has a relative segment", ErrInvalidKey, key)
		}
	}
	return nil
}

// validatePrefix accepts "" or anything that would be a valid key prefix.
func validatePrefix(p string) error {
	if p == "" {
		return nil
	}
	return ValidateKey(strings.TrimSuffix(p, "/") + "/x")
}

// normPrefix turns a URL path into "" or "a/b/".
func normPrefix(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	if err := ValidateKey(p); err != nil {
		return "", fmt.Errorf("objectstore: bad prefix: %w", err)
	}
	return p + "/", nil
}

// sizedBody returns a reader with a known length, buffering if size < 0.
func sizedBody(r io.Reader, size int64) (io.Reader, int64, error) {
	if size >= 0 {
		return io.LimitReader(r, size), size, nil
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, err
	}
	return bytes.NewReader(b), int64(len(b)), nil
}

// httpError builds an error from a non-2xx response, mapping 404 to
// ErrNotFound. It consumes (part of) the body.
func httpError(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %q: %w", op, key, ErrNotFound)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return fmt.Errorf("objectstore: %s %q: HTTP %d: %s", op, key, resp.StatusCode, msg)
}

func checkTTL(ttl time.Duration, max time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("objectstore: presign ttl must be positive")
	}
	if max > 0 && ttl > max {
		return fmt.Errorf("objectstore: presign ttl %s exceeds the provider maximum %s", ttl, max)
	}
	return nil
}
