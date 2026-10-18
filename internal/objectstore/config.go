package objectstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Environment variables read by ConfigFromEnv.
const (
	EnvURL            = "OF_OBJECT_STORE_URL"
	EnvTranscriptsURL = "OF_OBJECT_STORE_TRANSCRIPTS_URL"
	EnvArtifactsURL   = "OF_OBJECT_STORE_ARTIFACTS_URL"
	EnvAttachmentsURL = "OF_OBJECT_STORE_ATTACHMENTS_URL"
	EnvSnapshotsURL   = "OF_OBJECT_STORE_SNAPSHOTS_URL"
	EnvRetention      = "OF_OBJECT_STORE_RETENTION"
	EnvPresignTTL     = "OF_OBJECT_STORE_PRESIGN_TTL"
	EnvSigningKey     = "OF_OBJECT_STORE_SIGNING_KEY"
)

// DefaultPresignTTL is used when OF_OBJECT_STORE_PRESIGN_TTL is unset.
const DefaultPresignTTL = 15 * time.Minute

// Config is the object storage configuration.
type Config struct {
	// URL is the default store for every category.
	URL string
	// TranscriptsURL, ArtifactsURL and AttachmentsURL optionally map a
	// category to its own store; empty means URL.
	TranscriptsURL string
	ArtifactsURL   string
	AttachmentsURL string
	// SnapshotsURL maps the snapshots category to its own store; empty means
	// URL.
	SnapshotsURL string
	// Retention, when positive, is the age after which RunData.Prune
	// removes a run's objects. Zero keeps data forever.
	Retention time.Duration
	// PresignTTL is the lifetime of presigned URLs handed to clients.
	PresignTTL time.Duration
	// SigningKey signs local (file://) presigned URLs.
	SigningKey []byte
}

// ConfigFromEnv reads the OF_OBJECT_STORE_* variables. getenv defaults to
// os.Getenv. The default URL is file://$OF_DATA_DIR/objects, with OF_DATA_DIR
// falling back to $HOME/.openfactory.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := Config{
		URL:            getenv(EnvURL),
		TranscriptsURL: getenv(EnvTranscriptsURL),
		ArtifactsURL:   getenv(EnvArtifactsURL),
		AttachmentsURL: getenv(EnvAttachmentsURL),
		SnapshotsURL:   getenv(EnvSnapshotsURL),
		PresignTTL:     DefaultPresignTTL,
		SigningKey:     []byte(getenv(EnvSigningKey)),
	}
	if c.URL == "" {
		dir := getenv("OF_DATA_DIR")
		if dir == "" {
			home := getenv("HOME")
			if home == "" {
				home, _ = os.UserHomeDir()
			}
			dir = filepath.Join(home, ".openfactory")
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return Config{}, err
		}
		c.URL = "file://" + filepath.ToSlash(filepath.Join(abs, "objects"))
	}
	for _, d := range []struct {
		env string
		dst *time.Duration
	}{{EnvRetention, &c.Retention}, {EnvPresignTTL, &c.PresignTTL}} {
		v := getenv(d.env)
		if v == "" {
			continue
		}
		dur, err := time.ParseDuration(v)
		if err != nil || dur < 0 {
			return Config{}, fmt.Errorf("objectstore: %s=%q is not a non-negative duration", d.env, v)
		}
		*d.dst = dur
	}
	if c.PresignTTL == 0 {
		c.PresignTTL = DefaultPresignTTL
	}
	return c, nil
}

// OpenRunData opens the configured stores (each distinct URL once) and
// returns the RunData plus every store keyed by URL. opts.SigningKey is
// filled from the config when unset. When opts.RoutePrefix is empty, a local
// store opened for the default URL serves presigned links under "/objects/"
// and a local store opened only for one category serves them under
// "/objects-{category}/" (for example "/objects-artifacts/"), so the
// handlers from FileHandlers never collide.
func (c Config) OpenRunData(ctx context.Context, opts Options) (*RunData, map[string]Store, error) {
	if len(opts.SigningKey) == 0 {
		opts.SigningKey = c.SigningKey
	}
	stores := map[string]Store{}
	open := func(u string, cat Category) (Store, error) {
		if u == "" {
			u = c.URL
		}
		if s, ok := stores[u]; ok {
			return s, nil
		}
		o := opts
		if o.RoutePrefix == "" && cat != "" {
			o.RoutePrefix = "/objects-" + string(cat) + "/"
		}
		s, err := Open(ctx, u, o)
		if err != nil {
			return nil, err
		}
		stores[u] = s
		return s, nil
	}
	def, err := open(c.URL, "")
	if err != nil {
		return nil, nil, err
	}
	tr, err := open(c.TranscriptsURL, Transcripts)
	if err != nil {
		return nil, nil, err
	}
	ar, err := open(c.ArtifactsURL, Artifacts)
	if err != nil {
		return nil, nil, err
	}
	at, err := open(c.AttachmentsURL, Attachments)
	if err != nil {
		return nil, nil, err
	}
	sn, err := open(c.SnapshotsURL, Snapshots)
	if err != nil {
		return nil, nil, err
	}
	rd, err := NewRunData(def, tr, ar, at, sn)
	if err != nil {
		return nil, nil, err
	}
	return rd, stores, nil
}

// FileHandlers returns the presign handlers of every local store in stores,
// for mounting on the HTTP mux. Each local store must have its own
// Options.RoutePrefix (OpenRunData arranges this).
func FileHandlers(stores map[string]Store) []*Handler {
	var hs []*Handler
	for _, s := range stores {
		if fs, ok := s.(*FileStore); ok {
			hs = append(hs, fs.Handler())
		}
	}
	return hs
}
