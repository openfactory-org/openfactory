// Package gitcachekey names the git mirrors of the microVM worker cache. The
// worker agent and the runner in the VM use the same function, so the runner
// finds the mirror that the agent attached.
package gitcachekey

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// Key returns the key of the git mirror of a clone URL: the first 16 hex
// digits of the SHA-256 of the normalized URL. Normalized means: scheme and
// host in lower case, no user information, no query, no fragment, no
// trailing "/" and no trailing ".git". It returns "" for a URL that it cannot
// parse or that has no host.
func Key(cloneURL string) string {
	n := Normalize(cloneURL)
	if n == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:])[:16]
}

// Normalize returns the normalized form of a clone URL that Key hashes, or ""
// for a URL that it cannot parse or that has no host.
func Normalize(cloneURL string) string {
	u, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil || u.Host == "" {
		return ""
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.User = nil
	u.RawQuery, u.Fragment = "", ""
	p := strings.TrimSuffix(u.Path, "/")
	p = strings.TrimSuffix(p, ".git")
	u.Path, u.RawPath = p, ""
	return u.String()
}

// Valid reports whether k has the form of a key: 16 lower-case hex digits.
func Valid(k string) bool {
	if len(k) != 16 {
		return false
	}
	for _, c := range k {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
