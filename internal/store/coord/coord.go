// Package coord holds the helpers built on core.Coordinator that make several
// control-plane replicas safe on one database: the replica identity, the
// leader lease (Leader), distributed locks (Locker), and an in-process
// coordinator (Memory) for single-replica stores and test fakes.
package coord

import (
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ReplicaIDEnv names the environment variable holding the replica identity.
const ReplicaIDEnv = "OF_REPLICA_ID"

// ReplicaID returns the replica identity: explicit when non-empty, else
// $OF_REPLICA_ID, else the hostname (the pod name on Kubernetes), else "replica".
func ReplicaID(explicit string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(ReplicaIDEnv)); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "replica"
}

// NewHolder returns a holder id unique to this process: "<replica>/<random>".
// A restarted replica with the same name gets a different holder, so it can
// never renew leases of its previous life.
func NewHolder(replicaID string) string {
	return ReplicaID(replicaID) + "/" + core.NewID("")
}

// HolderReplica returns the replica part of a holder id.
func HolderReplica(holder string) string {
	r, _, _ := strings.Cut(holder, "/")
	return r
}

var fallbacks sync.Map // core.Store -> *Memory

// From returns the store's coordinator. A store that does not implement
// core.Coordinator (test fakes) gets a process-local Memory coordinator, one
// per store value, so every component of a process shares it. It is only
// correct with a single replica.
func From(s core.Store) core.Coordinator {
	if s == nil {
		return nil
	}
	if c, ok := s.(core.Coordinator); ok {
		return c
	}
	if !reflect.TypeOf(s).Comparable() {
		return NewMemory()
	}
	if c, ok := fallbacks.Load(s); ok {
		return c.(*Memory)
	}
	c, _ := fallbacks.LoadOrStore(s, NewMemory())
	return c.(*Memory)
}
