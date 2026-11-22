package runner

import "github.com/openfactory-org/openfactory/internal/gitcachekey"

// GitCacheKey returns the key of the git mirror of a clone URL. The microVM
// worker names its mirrors <key>.git with the same function. It returns ""
// for a URL that it cannot parse. See gitcachekey.Key.
func GitCacheKey(cloneURL string) string { return gitcachekey.Key(cloneURL) }
