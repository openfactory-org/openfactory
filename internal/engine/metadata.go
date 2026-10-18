package engine

import (
	"github.com/openfactory-org/openfactory/internal/worker"
)

// Run metadata limits. The rules are implemented once in internal/worker
// (shared with remote workers); these names are kept for engine callers.
const (
	MaxMetadataKeys     = worker.MaxMetadataKeys
	MaxMetadataKeyBytes = worker.MaxMetadataKeyLen
	MaxMetadataValueLen = worker.MaxMetadataValueLen
	// MetadataEnvPrefix is reserved for run metadata. Images, worker environments and
	// secrets must not set variables with this prefix.
	MetadataEnvPrefix = worker.MetadataEnvPrefix
)

// MetadataEnvName converts a metadata key to its environment variable name:
// OF_METADATA_ + uppercase key with '.' and '-' replaced by '_'.
func MetadataEnvName(key string) string { return worker.MetadataEnvName(key) }

// ValidateMetadata enforces the run metadata rules: at most 20 keys, keys of
// 1–64 bytes from [A-Za-z0-9._-], values of 0–256 bytes of UTF-8 without NUL,
// and no two keys mapping to the same environment variable name.
func ValidateMetadata(m map[string]string) error { return worker.ValidateMetadata(m) }

// MergeMetadata merges metadata layers for a child run. For each key the child
// value wins, then the batch value, then the parent value. The result is
// validated; an empty result is returned as nil.
func MergeMetadata(parent, batch, child map[string]string) (map[string]string, error) {
	out, err := worker.MergeMetadata(parent, batch, child)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out, nil
}

// MetadataEnv returns the OF_METADATA_* environment variables for run metadata.
// Callers validate metadata when it is accepted; invalid keys are still mapped.
func MetadataEnv(m map[string]string) map[string]string {
	env := make(map[string]string, len(m))
	for k, v := range m {
		env[MetadataEnvName(k)] = v
	}
	return env
}
