// Package worker holds helpers shared by the planner and remote workers:
// run metadata validation and environment mapping, capacity limiting and
// Prometheus text-format metrics.
package worker

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Run metadata limits.
const (
	MaxMetadataKeys     = 20
	MaxMetadataKeyLen   = 64
	MaxMetadataValueLen = 256
	// MetadataEnvPrefix prefixes metadata environment variables.
	MetadataEnvPrefix = "OF_METADATA_"
)

var metadataKeyRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// MetadataEnvName returns the environment variable for a metadata key:
// OF_METADATA_ + key uppercased with "." and "-" changed to "_".
func MetadataEnvName(key string) string {
	r := strings.NewReplacer(".", "_", "-", "_")
	return MetadataEnvPrefix + strings.ToUpper(r.Replace(key))
}

// ValidateMetadata enforces the metadata rules: at most 20 keys; keys of
// 1–64 bytes using A–Z, a–z, 0–9, ".", "_" and "-"; values of at most 256
// bytes of UTF-8 without NUL characters; and no two keys mapping to the same
// environment variable name. All violations are reported together.
func ValidateMetadata(meta map[string]string) error {
	var problems []string
	if len(meta) > MaxMetadataKeys {
		problems = append(problems, fmt.Sprintf("a run can have at most %d metadata keys, got %d", MaxMetadataKeys, len(meta)))
	}
	keys := sortedKeys(meta)
	envs := map[string]string{}
	for _, k := range keys {
		v := meta[k]
		switch {
		case len(k) == 0 || len(k) > MaxMetadataKeyLen:
			problems = append(problems, fmt.Sprintf("metadata key %q must be 1-%d bytes", k, MaxMetadataKeyLen))
		case !metadataKeyRe.MatchString(k):
			problems = append(problems, fmt.Sprintf("metadata key %q may contain only A-Z, a-z, 0-9, '.', '_' and '-'", k))
		}
		if len(v) > MaxMetadataValueLen {
			problems = append(problems, fmt.Sprintf("metadata value for %q exceeds %d bytes", k, MaxMetadataValueLen))
		}
		if !utf8.ValidString(v) {
			problems = append(problems, fmt.Sprintf("metadata value for %q is not valid UTF-8", k))
		}
		if strings.ContainsRune(v, 0) {
			problems = append(problems, fmt.Sprintf("metadata value for %q contains a NUL character", k))
		}
		if k != "" {
			env := MetadataEnvName(k)
			if other, ok := envs[env]; ok {
				problems = append(problems, fmt.Sprintf("metadata keys %q and %q both map to %s", other, k, env))
			} else {
				envs[env] = k
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", core.ErrInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// MetadataEnv validates meta and returns its OF_METADATA_* variables.
func MetadataEnv(meta map[string]string) (map[string]string, error) {
	if err := ValidateMetadata(meta); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[MetadataEnvName(k)] = v
	}
	return out, nil
}

// MergeMetadata merges run metadata for a child run: for each key the child
// value wins, then the batch (run_agents) value, then the inherited parent
// value. The merged result is validated (the key limit includes inherited keys).
func MergeMetadata(parent, batch, child map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(parent)+len(batch)+len(child))
	for _, m := range []map[string]string{parent, batch, child} {
		for k, v := range m {
			out[k] = v
		}
	}
	if err := ValidateMetadata(out); err != nil {
		return nil, err
	}
	return out, nil
}

// CheckNoMetadataEnv rejects environments (images, worker env, secrets) that
// try to set variables with the reserved OF_METADATA_ prefix.
func CheckNoMetadataEnv(env map[string]string) error {
	var bad []string
	for k := range env {
		if strings.HasPrefix(strings.ToUpper(k), MetadataEnvPrefix) {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("%w: the %s prefix is reserved for run metadata: %s", core.ErrInvalid, MetadataEnvPrefix, strings.Join(bad, ", "))
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
