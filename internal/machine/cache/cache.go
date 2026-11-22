// Package cache keeps bare git mirrors of the repositories of recent runs
// and builds one read-only ext4 disk per repository. The backend attaches
// only the disks of the session's repositories, so a factory never reads
// the repositories of another factory.
package cache

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/gitcachekey"
	"github.com/openfactory-org/openfactory/internal/machine/image"
)

// MaxAge is the age after which a mirror is fetched again.
const MaxAge = 30 * time.Minute

// Generations kept per repository.
const keepGenerations = 2

// Git runs git with extra environment and returns its output.
type Git func(ctx context.Context, env []string, args ...string) (string, error)

// ExecGit runs git without a shell. GIT_TERMINAL_PROMPT=0 makes a missing
// credential fail instead of waiting.
func ExecGit(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: git with agent-built arguments
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Cache is the git mirror cache under Dir (mirrors/ and images/).
type Cache struct {
	Dir  string
	Git  Git
	Mkfs image.Run
	// Helper is the credential helper command, for example
	// "/usr/local/bin/openfactory-runtime worker git-credential". Empty
	// means no credential (public repositories only).
	Helper string
	// HelperEnv returns the environment of the helper: the control URL,
	// the worker ID and the current access token.
	HelperEnv func() []string
	Log       *slog.Logger

	// locks serializes Ensure per key and keeps Prune away from a key that
	// Ensure uses.
	locks image.KeyedMutex
	mu    sync.Mutex
	urls  map[string]string
}

func (c *Cache) log() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

func (c *Cache) git() Git {
	if c.Git == nil {
		return ExecGit
	}
	return c.Git
}

func (c *Cache) lock(key string) func() { return c.locks.Lock(key) }

// MirrorDir returns the bare mirror of a key.
func (c *Cache) MirrorDir(key string) string { return filepath.Join(c.Dir, "mirrors", key+".git") }

func (c *Cache) imageDir(key string) string { return filepath.Join(c.Dir, "images", key) }

// Latest returns the newest disk of a key, or "" when there is none.
func (c *Cache) Latest(key string) string {
	gens := c.generations(key)
	if len(gens) == 0 {
		return ""
	}
	return gens[len(gens)-1]
}

// generations returns the disks of a key, oldest first.
func (c *Cache) generations(key string) []string {
	matches, _ := filepath.Glob(filepath.Join(c.imageDir(key), "*.ext4"))
	sort.Slice(matches, func(i, j int) bool {
		a, _ := strconv.ParseInt(strings.TrimSuffix(filepath.Base(matches[i]), ".ext4"), 10, 64)
		b, _ := strconv.ParseInt(strings.TrimSuffix(filepath.Base(matches[j]), ".ext4"), 10, 64)
		return a < b
	})
	return matches
}

func (c *Cache) gitArgs(args ...string) []string {
	if c.Helper == "" {
		return args
	}
	return append([]string{"-c", "credential.helper=", "-c", "credential.helper=!" + c.Helper}, args...)
}

func (c *Cache) env() []string {
	if c.HelperEnv == nil {
		return nil
	}
	return c.HelperEnv()
}

// Ensure makes sure that the mirror of cloneURL exists, is younger than
// MaxAge, and has a disk. It is slow: call it in the background and never
// block a task on it.
func (c *Cache) Ensure(ctx context.Context, key, cloneURL string) error {
	if gitcachekey.Key(cloneURL) != key {
		return fmt.Errorf("git cache key %q does not match %s", key, gitcachekey.Normalize(cloneURL))
	}
	defer c.lock(key)()
	c.mu.Lock()
	if c.urls == nil {
		c.urls = map[string]string{}
	}
	c.urls[key] = cloneURL
	c.mu.Unlock()
	if latest := c.Latest(key); latest != "" {
		if fi, err := os.Stat(latest); err == nil && time.Since(fi.ModTime()) < MaxAge {
			return nil
		}
	}
	mirror := c.MirrorDir(key)
	g := c.git()
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(mirror), 0o700); err != nil {
			return err
		}
		_ = os.RemoveAll(mirror)
		// Not --mirror: that pulls refs/pull/* on GitHub.
		if _, err := g(ctx, c.env(), c.gitArgs("clone", "--bare", "--quiet", "--", cloneURL, mirror)...); err != nil {
			_ = os.RemoveAll(mirror)
			return err
		}
		for _, spec := range []string{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"} {
			if _, err := g(ctx, nil, "--git-dir", mirror, "config", "--add", "remote.origin.fetch", spec); err != nil {
				return err
			}
		}
		_, _ = g(ctx, nil, "--git-dir", mirror, "config", "--unset-all", "remote.origin.fetch", `^\+refs/heads/\*:refs/remotes/origin/\*$`)
	} else if _, err := g(ctx, c.env(), c.gitArgs("--git-dir", mirror, "fetch", "--prune", "--quiet", "origin")...); err != nil {
		return err
	}
	return c.build(ctx, key)
}

func (c *Cache) build(ctx context.Context, key string) error {
	mirror := c.MirrorDir(key)
	var bytes int64
	var entries int
	_ = filepath.Walk(mirror, func(_ string, fi os.FileInfo, err error) error {
		if err == nil {
			entries++
			if fi.Mode().IsRegular() {
				bytes += (fi.Size() + 4095) &^ 4095
			}
		}
		return nil
	})
	dir := c.imageDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gen := strconv.FormatInt(time.Now().UnixNano(), 10)
	tmp := filepath.Join(dir, gen+".tmp")
	run := c.Mkfs
	if run == nil {
		run = image.ExecRun
	}
	if _, err := run(ctx, "mkfs.ext4", image.MkfsImageArgs("of-gc", mirror, tmp, image.Stats{Entries: entries, Bytes: bytes})...); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, gen+".ext4")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	gens := c.generations(key)
	for len(gens) > keepGenerations {
		// A live VM keeps its hard link; removing the cache entry is safe.
		_ = os.Remove(gens[0])
		gens = gens[1:]
	}
	c.log().Info("git cache disk built", "key", key, "bytes", bytes)
	return nil
}

// Refresh runs Ensure for every repository that Ensure saw.
func (c *Cache) Refresh(ctx context.Context) {
	c.mu.Lock()
	urls := map[string]string{}
	for k, v := range c.urls {
		urls[k] = v
	}
	c.mu.Unlock()
	for k, u := range urls {
		if ctx.Err() != nil {
			return
		}
		if err := c.Ensure(ctx, k, u); err != nil {
			c.log().Warn("git cache refresh", "key", k, "err", err)
		}
	}
}

// Prune removes the least recently used repositories until the cache uses
// at most maxBytes. inUse protects keys of live VMs. A key that Ensure
// holds (a clone, fetch or disk build in progress) is skipped.
func (c *Cache) Prune(maxBytes int64, inUse func(key string) bool) int64 {
	entries, _ := os.ReadDir(filepath.Join(c.Dir, "mirrors"))
	type repo struct {
		key  string
		size int64
		used time.Time
	}
	var repos []repo
	var total int64
	for _, e := range entries {
		key := strings.TrimSuffix(e.Name(), ".git")
		if !gitcachekey.Valid(key) {
			continue
		}
		var size int64
		var used time.Time
		for _, d := range []string{c.MirrorDir(key), c.imageDir(key)} {
			_ = filepath.Walk(d, func(_ string, fi os.FileInfo, err error) error {
				if err == nil && fi.Mode().IsRegular() {
					size += image.DiskUsage(fi)
					if fi.ModTime().After(used) {
						used = fi.ModTime()
					}
				}
				return nil
			})
		}
		total += size
		repos = append(repos, repo{key, size, used})
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].used.Before(repos[j].used) })
	var freed int64
	for _, r := range repos {
		if total <= maxBytes {
			break
		}
		if inUse != nil && inUse(r.key) {
			continue
		}
		unlock, ok := c.locks.TryLock(r.key)
		if !ok {
			continue
		}
		_ = os.RemoveAll(c.MirrorDir(r.key))
		_ = os.RemoveAll(c.imageDir(r.key))
		c.mu.Lock()
		delete(c.urls, r.key)
		c.mu.Unlock()
		unlock()
		total -= r.size
		freed += r.size
	}
	return freed
}

// Credential is the answer of GET /api/v1/workers/{id}/git-credential.
type Credential struct {
	Username  string    `json:"username"`
	Password  string    `json:"password"` //nolint:gosec // G117: a short-lived read-only token for git
	ExpiresAt time.Time `json:"expires_at"`
}

// ErrNoCredential means the control plane refused a credential for the
// repository (revoked worker, a repository not sent to this worker, or a
// forge that cannot scope a read-only token). The mirror is skipped.
var ErrNoCredential = errors.New("no git credential for this repository")

// FetchCredential asks the control plane for a read-only credential of repo.
func FetchCredential(ctx context.Context, hc *http.Client, controlURL, workerID, token, repo string) (Credential, error) {
	var cred Credential
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	u := strings.TrimRight(controlURL, "/") + "/api/v1/workers/" + url.PathEscape(workerID) + "/git-credential?repo=" + url.QueryEscape(repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return cred, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized:
		return cred, fmt.Errorf("%w: %s", ErrNoCredential, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return cred, fmt.Errorf("git credential: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, &cred); err != nil || cred.Password == "" {
		return cred, fmt.Errorf("git credential: invalid answer")
	}
	return cred, nil
}

// Environment variables of the credential helper.
const (
	EnvControlURL  = "OF_WORKER_CONTROL_URL"
	EnvWorkerID    = "OF_WORKER_ID"
	EnvAccessToken = "OF_WORKER_ACCESS_TOKEN" //nolint:gosec // G101: a variable name
)

// HelperMain is the git credential helper ("worker git-credential get"). It
// reads the git credential request on in and writes username and password
// to out. Operations other than get are ignored.
func HelperMain(ctx context.Context, op string, in io.Reader, out io.Writer, getenv func(string) string) error {
	if op != "get" {
		return nil
	}
	fields := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			fields[k] = v
		}
	}
	token := getenv(EnvAccessToken)
	if token == "" {
		return nil // no token: git tries without a credential
	}
	repo := (&url.URL{Scheme: fields["protocol"], Host: fields["host"], Path: "/" + strings.TrimPrefix(fields["path"], "/")}).String()
	cred, err := FetchCredential(ctx, nil, getenv(EnvControlURL), getenv(EnvWorkerID), token, repo)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "username=%s\npassword=%s\n", cred.Username, cred.Password)
	return err
}
