package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
)

// GitCacheKey returns the key of the git mirror of a clone URL: the first 16
// hex digits of the SHA-256 of the normalized URL. Normalized means: scheme
// and host in lower case, no user information, no trailing "/" and no
// trailing ".git". The microVM worker names its mirrors <key>.git with the
// same function. It returns "" for a URL that it cannot parse.
func GitCacheKey(cloneURL string) string {
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
	sum := sha256.Sum256([]byte(u.String()))
	return hex.EncodeToString(sum[:])[:16]
}
