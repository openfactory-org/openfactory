package plugin

import (
	"path/filepath"
	"sort"
	"strings"
)

// baseEnv names the variables that every plugin process gets when the server
// has them. The handshake variables of go-plugin (OF_PLUGIN, PLUGIN_*) are
// added by go-plugin itself.
var baseEnv = []string{"PATH", "HOME", "TMPDIR", "LANG", "TZ"}

// neverEnv returns true for a variable that a plugin never gets, also when an
// operator lists it in OF_PLUGIN_ENV_ALLOW or OF_PLUGIN_ENV. These variables hold the keys and
// the credentials of the server itself.
func neverEnv(name string) bool {
	switch {
	case name == "OF_DATABASE_URL",
		strings.HasPrefix(name, "OF_SECRET_KEY"),
		strings.HasPrefix(name, "OF_SIGNING_KEY"),
		strings.HasPrefix(name, "OF_OBJECT_STORE_"),
		strings.HasSuffix(name, "_API_KEY"),
		name == "OF_OIDC_CLIENT_SECRET":
		return true
	}
	return false
}

// pluginNameFromPath returns the plugin name that the file name of an
// executable gives: the name without the prefix openfactory-plugin-.
func pluginNameFromPath(path string) string {
	return strings.TrimPrefix(filepath.Base(path), BinaryPrefix)
}

// pluginEnv builds the environment of one plugin process as KEY=value pairs,
// sorted by key. It holds the variables of baseEnv, the variables in allow
// (for all plugins) and the variables in perPlugin for this plugin name. It reads values with lookup. A variable that
// lookup does not find is left out. A variable that neverEnv matches is left
// out, also when it is in a list.
func pluginEnv(path string, allow []string, perPlugin map[string][]string, lookup func(string) (string, bool)) []string {
	names := map[string]bool{}
	for _, n := range baseEnv {
		names[n] = true
	}
	for _, n := range allow {
		if n = strings.TrimSpace(n); n != "" {
			names[n] = true
		}
	}
	for _, n := range perPlugin[pluginNameFromPath(path)] {
		if n = strings.TrimSpace(n); n != "" {
			names[n] = true
		}
	}
	var out []string
	for n := range names {
		if neverEnv(n) {
			continue
		}
		if v, ok := lookup(n); ok {
			out = append(out, n+"="+v)
		}
	}
	sort.Strings(out)
	return out
}
