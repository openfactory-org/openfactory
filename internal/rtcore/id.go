package rtcore

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewID returns a random, URL-safe identifier with the given prefix, e.g. NewID("run") => "run_k3j...".
func NewID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	s := strings.ToLower(idEncoding.EncodeToString(b[:]))
	if prefix == "" {
		return s
	}
	return prefix + "_" + s
}

// ArtifactsDir is the directory, relative to the sandbox workspace
// (Sandbox.WorkDir), whose files are collected as run artifacts after every
// turn. Runs see its absolute path in $OF_ARTIFACTS_DIR.
const ArtifactsDir = ".openfactory/artifacts"

// EnvArtifactsDir names the environment variable holding the absolute
// artifacts directory of a run's sandbox.
const EnvArtifactsDir = "OF_ARTIFACTS_DIR"
