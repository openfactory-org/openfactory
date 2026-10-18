package objectstore

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// metaDir holds per-object metadata sidecars inside a file store root. Keys
// whose first segment is metaDir are rejected.
const metaDir = ".ofmeta"

// FileStore keeps objects as files under a root directory. Presigned URLs are
// HMAC-signed links served by the store's Handler.
type FileStore struct {
	root        string
	signingKey  []byte
	baseURL     string
	routePrefix string
	maxPut      int64
	now         func() time.Time
}

type fileMeta struct {
	ContentType string `json:"content_type,omitempty"`
}

func openFile(rest string, opts Options) (*FileStore, error) {
	// file:///abs/path or file://localhost/abs/path; a query string is ignored.
	p, _, _ := strings.Cut(rest, "?")
	p = strings.TrimPrefix(p, "localhost")
	if p == "" || !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("objectstore: file URL must be file:///absolute/path, got file://%s", rest)
	}
	if dec, err := url.PathUnescape(p); err == nil {
		p = dec
	}
	return NewFileStore(filepath.FromSlash(p), opts)
}

// NewFileStore returns a local-disk store rooted at dir, creating it if
// needed.
func NewFileStore(dir string, opts Options) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // the root directory comes from the operator configuration
		return nil, fmt.Errorf("objectstore: create %s: %w", dir, err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	rp := opts.RoutePrefix
	if rp == "" {
		rp = "/objects/"
	}
	if !strings.HasPrefix(rp, "/") {
		rp = "/" + rp
	}
	if !strings.HasSuffix(rp, "/") {
		rp += "/"
	}
	maxPut := opts.MaxPutBytes
	if maxPut <= 0 {
		maxPut = 5 << 30
	}
	return &FileStore{
		root:        abs,
		signingKey:  opts.SigningKey,
		baseURL:     strings.TrimRight(opts.BaseURL, "/"),
		routePrefix: rp,
		maxPut:      maxPut,
		now:         opts.now,
	}, nil
}

// Root returns the store's directory.
func (s *FileStore) Root() string { return s.root }

func (s *FileStore) checkKey(key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if key == metaDir || strings.HasPrefix(key, metaDir+"/") {
		return fmt.Errorf("%w: %q is reserved", ErrInvalidKey, key)
	}
	return nil
}

func (s *FileStore) dataPath(key string) string {
	return filepath.Join(s.root, filepath.FromSlash(key))
}
func (s *FileStore) metaPath(key string) string {
	return filepath.Join(s.root, metaDir, filepath.FromSlash(key)+".json")
}

// writeAtomic writes r to dst via a temp file and rename.
func writeAtomic(dst string, r io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return 0, err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return n, err
	}
	return n, nil
}

// Put implements Store.
func (s *FileStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if err := s.checkKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	src := r
	if size >= 0 {
		src = io.LimitReader(r, size)
	}
	n, err := writeAtomic(s.dataPath(key), &ctxReader{ctx: ctx, r: src})
	if err != nil {
		return fmt.Errorf("objectstore: put %q: %w", key, err)
	}
	if size >= 0 && n != size {
		_ = os.Remove(s.dataPath(key))
		return fmt.Errorf("objectstore: put %q: short body (%d of %d bytes)", key, n, size)
	}
	mb, _ := json.Marshal(fileMeta{ContentType: contentType})
	if _, err := writeAtomic(s.metaPath(key), strings.NewReader(string(mb))); err != nil {
		return fmt.Errorf("objectstore: put %q metadata: %w", key, err)
	}
	return nil
}

func (s *FileStore) info(key string, fi os.FileInfo) ObjectInfo {
	ct := ""
	if b, err := os.ReadFile(s.metaPath(key)); err == nil {
		var m fileMeta
		if json.Unmarshal(b, &m) == nil {
			ct = m.ContentType
		}
	}
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(key))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	return ObjectInfo{
		Key:          key,
		Size:         fi.Size(),
		ContentType:  ct,
		LastModified: fi.ModTime().UTC(),
		ETag:         fmt.Sprintf("%x-%x", fi.ModTime().UnixNano(), fi.Size()),
	}
}

// Get implements Store.
func (s *FileStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	f, info, err := s.open(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	return f, info, nil
}

func (s *FileStore) open(key string) (*os.File, ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, ObjectInfo{}, err
	}
	f, err := os.Open(s.dataPath(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ObjectInfo{}, fmt.Errorf("get %q: %w", key, ErrNotFound)
		}
		return nil, ObjectInfo{}, fmt.Errorf("objectstore: get %q: %w", key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ObjectInfo{}, err
	}
	if fi.IsDir() {
		f.Close()
		return nil, ObjectInfo{}, fmt.Errorf("get %q: %w", key, ErrNotFound)
	}
	return f, s.info(key, fi), nil
}

// Stat implements Store.
func (s *FileStore) Stat(_ context.Context, key string) (ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return ObjectInfo{}, err
	}
	fi, err := os.Stat(s.dataPath(key))
	if err != nil || fi.IsDir() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return ObjectInfo{}, fmt.Errorf("stat %q: %w", key, ErrNotFound)
		}
		return ObjectInfo{}, fmt.Errorf("objectstore: stat %q: %w", key, err)
	}
	return s.info(key, fi), nil
}

// Delete implements Store. Empty parent directories are removed.
func (s *FileStore) Delete(_ context.Context, key string) error {
	if err := s.checkKey(key); err != nil {
		return err
	}
	if err := os.Remove(s.dataPath(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("objectstore: delete %q: %w", key, err)
	}
	_ = os.Remove(s.metaPath(key))
	s.pruneDirs(filepath.Dir(s.dataPath(key)), s.root)
	s.pruneDirs(filepath.Dir(s.metaPath(key)), filepath.Join(s.root, metaDir))
	return nil
}

func (s *FileStore) pruneDirs(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// List implements Store.
func (s *FileStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	// Walk only the deepest directory fully named by prefix.
	start := s.root
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		start = filepath.Join(s.root, filepath.FromSlash(prefix[:i]))
	}
	var out []ObjectInfo
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, rerr := filepath.Rel(s.root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == metaDir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tmp-") || !strings.HasPrefix(rel, prefix) {
			return nil
		}
		fi, ierr := d.Info()
		if ierr != nil {
			if errors.Is(ierr, fs.ErrNotExist) {
				return nil // removed concurrently
			}
			return ierr
		}
		out = append(out, s.info(rel, fi))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: list %q: %w", prefix, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// PresignGet implements Store.
func (s *FileStore) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	return s.presign(http.MethodGet, key, ttl, "")
}

// PresignPut implements Store.
func (s *FileStore) PresignPut(_ context.Context, key string, ttl time.Duration, contentType string) (string, error) {
	return s.presign(http.MethodPut, key, ttl, contentType)
}

func (s *FileStore) presign(method, key string, ttl time.Duration, contentType string) (string, error) {
	if err := s.checkKey(key); err != nil {
		return "", err
	}
	if err := checkTTL(ttl, 0); err != nil {
		return "", err
	}
	if len(s.signingKey) == 0 {
		return "", fmt.Errorf("%w: file store has no signing key", ErrPresignUnsupported)
	}
	exp := s.now().Add(ttl).Unix()
	q := url.Values{}
	q.Set("exp", strconv.FormatInt(exp, 10))
	if contentType != "" {
		q.Set("ct", contentType)
	}
	q.Set("sig", s.sign(method, key, exp, contentType))
	return s.baseURL + s.routePrefix + escapePath(key) + "?" + q.Encode(), nil
}

func (s *FileStore) sign(method, key string, exp int64, contentType string) string {
	m := hmac.New(sha256.New, s.signingKey)
	fmt.Fprintf(m, "%s\n%s\n%d\n%s", method, key, exp, contentType)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// escapePath escapes each "/"-separated segment.
func escapePath(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Handler serves a FileStore's presigned URLs.
func (s *FileStore) Handler() *Handler { return &Handler{store: s} }

// Handler serves GET and PUT on presigned local URLs:
//
//	GET {RoutePrefix}{key...}?exp=…&sig=…
//	PUT {RoutePrefix}{key...}?exp=…[&ct=…]&sig=…
//
// It checks the signature and expiry and rejects path traversal. It never
// caches: responses carry "Cache-Control: private, no-store".
type Handler struct{ store *FileStore }

// Register mounts the handler on mux at the store's RoutePrefix.
func (h *Handler) Register(mux *http.ServeMux) {
	p := h.store.routePrefix
	mux.Handle("GET "+p+"{key...}", h)
	mux.Handle("HEAD "+p+"{key...}", h)
	mux.Handle("PUT "+p+"{key...}", h)
}

// ServeHTTP implements http.Handler. The object key is taken from the "key"
// path value when routed by Register, else from the path after RoutePrefix.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := h.store
	key := r.PathValue("key")
	if key == "" {
		key = strings.TrimPrefix(r.URL.Path, s.routePrefix)
	}
	if err := s.checkKey(key); err != nil {
		http.Error(w, "invalid object key", http.StatusBadRequest)
		return
	}
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPut {
		w.Header().Set("Allow", "GET, HEAD, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(s.signingKey) == 0 {
		http.Error(w, "presigned URLs disabled", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		http.Error(w, "missing or bad exp", http.StatusForbidden)
		return
	}
	ct := q.Get("ct")
	want := s.sign(method, key, exp, ct)
	if !hmac.Equal([]byte(want), []byte(q.Get("sig"))) {
		http.Error(w, "signature mismatch", http.StatusForbidden)
		return
	}
	if s.now().Unix() > exp {
		http.Error(w, "URL expired", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	switch method {
	case http.MethodGet:
		f, info, err := s.open(key)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", info.ContentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(path.Base(key)))
		http.ServeContent(w, r, "", info.LastModified, f)
	case http.MethodPut:
		if ct != "" && r.Header.Get("Content-Type") != ct {
			http.Error(w, "Content-Type does not match the signed value", http.StatusForbidden)
			return
		}
		if r.ContentLength > s.maxPut {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		body := http.MaxBytesReader(w, r.Body, s.maxPut)
		if err := s.Put(r.Context(), key, body, r.ContentLength, r.Header.Get("Content-Type")); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "write failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// ctxReader aborts reads once ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// NewSigningKey returns 32 random bytes, hex-encoded, suitable for
// OF_OBJECT_STORE_SIGNING_KEY.
func NewSigningKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
