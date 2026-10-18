package core

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
