// Package direct implements remote workers: processes that connect to the
// control plane over an outbound WebSocket, so no inbound firewall port is
// needed on the worker host. A worker runs the tasks that the control plane
// assigns, and the runner in each task talks to the control plane directly.
//
// Server side, Hub accepts worker connections at GET /api/v1/workers/connect
// (authenticated with a team API key as a bearer token), records workers and
// their heartbeats through rtcore.Store.UpsertWorker, and gives the dispatcher
// one rtcore.TaskBackend for each worker ID (HubBackend). Worker side,
// RunWorkerV2 connects (reconnecting with exponential backoff), registers its
// id/name/labels/capacity/backend and runs tasks with a local
// rtcore.TaskBackend (kubernetes, or local for development).
//
// There is one wire protocol, version 2 (WebSocket subprotocol
// "openfactory.worker.v2"). protocolv2.go describes it. Version 1, which
// sent sandbox operations (exec, read and write file) to the worker, was
// removed. A worker that offers only version 1 is refused.
package direct

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

const (
	// ConnectPath is the worker connection endpoint.
	ConnectPath = "/api/v1/workers/connect"
)

// Message types.
const (
	typeHello     = "hello"
	typeWelcome   = "welcome"
	typeHeartbeat = "heartbeat"
	typeRequest   = "request"
	typeResponse  = "response"
	typeEvent     = "event"

	typeAuthChallenge = "auth.challenge"
	typeAuthResponse  = "auth.response"
)

// Error codes.
const (
	codeNotFound   = "not_found"
	codeAtCapacity = "at_capacity"
	codeInternal   = "internal"
)

// ErrAtCapacity is returned when the selected worker has no free slot.
var ErrAtCapacity = errors.New("worker at capacity")

// ErrNoWorker is returned when no suitable worker is connected.
var ErrNoWorker = errors.New("no connected worker")

type message struct {
	Type   string          `json:"type"`
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// Hello describes a connected worker. A worker sends HelloV2 when it connects;
// the hub keeps the fields below.
type Hello struct {
	WorkerID string            `json:"worker_id"`
	Name     string            `json:"name,omitempty"`
	Backend  string            `json:"backend"`
	Labels   map[string]string `json:"labels,omitempty"`
	Capacity int               `json:"capacity"` // 0 = unlimited
	Active   int               `json:"active"`
	Version  string            `json:"version,omitempty"`
}

// Welcome is the hub's reply to Hello.
type Welcome struct {
	ConnectionID string `json:"connection_id"`
	HeartbeatMS  int64  `json:"heartbeat_ms"`
}

// Heartbeat reports worker load.
type Heartbeat struct {
	Active   int `json:"active"`
	Capacity int `json:"capacity"`
}

var workerIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ReservedWorkerIDs cannot be used by remote workers: they name built-in
// backends.
var ReservedWorkerIDs = []string{"server", "local", "kubernetes", "direct", "managed"}

// ValidateWorkerID checks a worker id: 1–64 characters of letters, digits,
// ".", "_" and "-", starting with a letter or digit; ids that name built-in
// worker hosts or start with "openfactory" are reserved.
func ValidateWorkerID(id string) error {
	if !workerIDRe.MatchString(id) {
		return fmt.Errorf("%w: worker id %q must be 1-64 characters of letters, digits, '.', '_' or '-'", rtcore.ErrInvalid, id)
	}
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "openfactory") {
		return fmt.Errorf("%w: worker ids starting with \"openfactory\" are reserved", rtcore.ErrInvalid)
	}
	for _, r := range ReservedWorkerIDs {
		if lower == r {
			return fmt.Errorf("%w: worker id %q is reserved", rtcore.ErrInvalid, id)
		}
	}
	return nil
}
