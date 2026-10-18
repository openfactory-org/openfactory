// Package wire holds what the plugin host (internal/plugin) and the plugin
// SDK (pkg/plugin/sdk) share: the go-plugin handshake, the plugin set,
// conversions between core types and protocol messages, and error mapping.
//
// EXPERIMENTAL: this package is not covered by the v1 compatibility promise.
// Plugin authors use pkg/plugin/sdk; wire is public only
// because the SDK exposes some of its values (GRPCPlugin, Handshake).
package wire

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	pb "github.com/openfactory-org/openfactory/pkg/plugin/pb"
)

// ProtocolVersion is the plugin protocol version (proto package openfactory.plugin.v1).
const ProtocolVersion = 1

// PluginKey is the single entry of the go-plugin plugin set.
const PluginKey = "openfactory"

// Capability names reported in PluginInfo.capabilities.
const (
	CapForge     = "forge"
	CapResponder = "responder"
	CapIngress   = "ingress"
)

// MaxMessageBytes bounds gRPC messages in both directions (webhook bodies,
// definition trees).
const MaxMessageBytes = 128 << 20

// Handshake is the go-plugin handshake. The host sets OF_PLUGIN in the plugin's
// environment; a binary started without it refuses to run.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "OF_PLUGIN",
	MagicCookieValue: "openfactory-plugin-7c1f3f0e9a5d4b2c",
}

// Conn is what the host gets from go-plugin for a running plugin.
type Conn struct {
	Broker *plugin.GRPCBroker
	Conn   *grpc.ClientConn
}

// GRPCPlugin is the go-plugin GRPCPlugin for both sides. Register is used by
// the plugin process; the host only uses GRPCClient.
type GRPCPlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Register func(broker *plugin.GRPCBroker, s *grpc.Server) error
}

// GRPCServer implements plugin.GRPCPlugin.
func (p *GRPCPlugin) GRPCServer(b *plugin.GRPCBroker, s *grpc.Server) error {
	if p.Register == nil {
		return errors.New("plugin: no services registered")
	}
	return p.Register(b, s)
}

// GRPCClient implements plugin.GRPCPlugin.
func (p *GRPCPlugin) GRPCClient(_ context.Context, b *plugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &Conn{Broker: b, Conn: c}, nil
}

// PluginSet returns the go-plugin plugin set.
func PluginSet(p *GRPCPlugin) plugin.PluginSet { return plugin.PluginSet{PluginKey: p} }

// ---------- conversions ----------

// SourceToPB converts a core.SourceRef.
func SourceToPB(s core.SourceRef) *pb.SourceRef {
	return &pb.SourceRef{Provider: s.Provider, Kind: s.Kind, Url: s.URL, Repo: s.Repo, Number: int64(s.Number),
		Channel: s.Channel, ThreadId: s.ThreadID, ExternalId: s.ExternalID, Title: s.Title, ThreadRef: s.ThreadRef}
}

// SourceFromPB converts a pb.SourceRef (nil yields the zero value).
func SourceFromPB(s *pb.SourceRef) core.SourceRef {
	if s == nil {
		return core.SourceRef{}
	}
	return core.SourceRef{Provider: s.Provider, Kind: s.Kind, URL: s.Url, Repo: s.Repo, Number: int(s.Number),
		Channel: s.Channel, ThreadID: s.ThreadId, ExternalID: s.ExternalId, Title: s.Title, ThreadRef: s.ThreadRef}
}

// ReviewThreadToPB converts a core.ReviewThread.
func ReviewThreadToPB(t *core.ReviewThread) *pb.ReviewThread {
	if t == nil {
		return nil
	}
	out := &pb.ReviewThread{Ref: t.Ref, Path: t.Path, Line: int64(t.Line), Resolved: t.Resolved}
	for _, c := range t.Comments {
		pc := &pb.ThreadComment{Id: c.ID, Author: c.Author, Body: c.Body, Path: c.Path, Line: int64(c.Line)}
		if !c.CreatedAt.IsZero() {
			pc.CreatedAt = timestamppb.New(c.CreatedAt)
		}
		out.Comments = append(out.Comments, pc)
	}
	return out
}

// ReviewThreadFromPB converts a pb.ReviewThread.
func ReviewThreadFromPB(t *pb.ReviewThread) *core.ReviewThread {
	if t == nil {
		return nil
	}
	out := &core.ReviewThread{Ref: t.Ref, Path: t.Path, Line: int(t.Line), Resolved: t.Resolved}
	for _, c := range t.Comments {
		tc := core.ThreadComment{ID: c.Id, Author: c.Author, Body: c.Body, Path: c.Path, Line: int(c.Line)}
		if c.CreatedAt != nil {
			tc.CreatedAt = c.CreatedAt.AsTime().UTC()
		}
		out.Comments = append(out.Comments, tc)
	}
	return out
}

// EventToPB converts a core.Event. Attributes are encoded as JSON.
func EventToPB(ev *core.Event) (*pb.Event, error) {
	var attrs []byte
	if ev.Attributes != nil {
		b, err := json.Marshal(ev.Attributes)
		if err != nil {
			return nil, err
		}
		attrs = b
	}
	out := &pb.Event{
		Id: ev.ID, TeamId: ev.TeamID, FactoryId: ev.FactoryID, Provider: ev.Provider, Type: ev.Type, DeliveryId: ev.DeliveryID,
		Actor:          &pb.Actor{Provider: ev.Actor.Provider, ExternalId: ev.Actor.ExternalID, Handle: ev.Actor.Handle, IsBot: ev.Actor.IsBot, UserId: ev.Actor.UserID},
		Source:         SourceToPB(ev.Source),
		AttributesJson: attrs,
		Payload:        ev.Payload,
		Text:           ev.Text,
		ActionKey:      ev.ActionKey,
	}
	if !ev.ReceivedAt.IsZero() {
		out.ReceivedAt = timestamppb.New(ev.ReceivedAt)
	}
	return out, nil
}

// EventFromPB converts a pb.Event. JSON attribute values are normalized so
// that in-process consumers see the same Go types integrations produce:
// lists of strings become []string and integral numbers become int.
func EventFromPB(e *pb.Event) (*core.Event, error) {
	ev := &core.Event{
		ID: e.Id, TeamID: e.TeamId, FactoryID: e.FactoryId, Provider: e.Provider, Type: e.Type, DeliveryID: e.DeliveryId,
		Source: SourceFromPB(e.Source), Text: e.Text, Attributes: map[string]any{},
		ActionKey: e.ActionKey,
	}
	if a := e.Actor; a != nil {
		ev.Actor = core.Actor{Provider: a.Provider, ExternalID: a.ExternalId, Handle: a.Handle, IsBot: a.IsBot, UserID: a.UserId}
	}
	if len(e.AttributesJson) > 0 {
		var m map[string]any
		if err := json.Unmarshal(e.AttributesJson, &m); err != nil {
			return nil, err
		}
		for k, v := range m {
			ev.Attributes[k] = NormalizeJSONValue(v)
		}
	}
	if len(e.Payload) > 0 {
		ev.Payload = json.RawMessage(e.Payload)
	}
	if e.ReceivedAt != nil {
		ev.ReceivedAt = e.ReceivedAt.AsTime().UTC()
	} else {
		ev.ReceivedAt = time.Now().UTC()
	}
	if ev.ID == "" {
		ev.ID = core.NewID("evt")
	}
	return ev, nil
}

// NormalizeJSONValue maps a decoded JSON value to the Go types integrations
// use for event attributes.
func NormalizeJSONValue(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int(t)
		}
		return t
	case []any:
		strs := make([]string, 0, len(t))
		for _, x := range t {
			s, ok := x.(string)
			if !ok {
				out := make([]any, len(t))
				for i, y := range t {
					out[i] = NormalizeJSONValue(y)
				}
				return out
			}
			strs = append(strs, s)
		}
		return strs
	case map[string]any:
		for k, x := range t {
			t[k] = NormalizeJSONValue(x)
		}
		return t
	}
	return v
}

// PRToPB converts a core.PullRequestRef.
func PRToPB(p *core.PullRequestRef) *pb.PullRequestRef {
	if p == nil {
		return &pb.PullRequestRef{}
	}
	return &pb.PullRequestRef{Forge: p.Forge, Repo: p.Repo, Number: int64(p.Number), Url: p.URL, Branch: p.Branch, State: p.State}
}

// PRFromPB converts a pb.PullRequestRef.
func PRFromPB(p *pb.PullRequestRef) *core.PullRequestRef {
	if p == nil {
		return nil
	}
	return &core.PullRequestRef{Forge: p.Forge, Repo: p.Repo, Number: int(p.Number), URL: p.Url, Branch: p.Branch, State: p.State}
}

// IntegrationToPB converts a core.Integration (secrets are references only).
func IntegrationToPB(in core.Integration) *pb.Integration {
	return &pb.Integration{Id: in.ID, TeamId: in.TeamID, Provider: in.Provider, Name: in.Name, Config: in.Config, SecretRef: in.SecretRef, FactoryId: in.FactoryID}
}

// IntegrationFromPB converts a pb.Integration.
func IntegrationFromPB(in *pb.Integration) core.Integration {
	cfg := in.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	return core.Integration{ID: in.Id, TeamID: in.TeamId, Provider: in.Provider, Name: in.Name, Config: cfg, SecretRef: in.SecretRef, FactoryID: in.FactoryId}
}

// OutboundToPB converts a core.OutboundMessage.
func OutboundToPB(m core.OutboundMessage) *pb.OutboundMessage {
	out := &pb.OutboundMessage{WorkItemId: m.WorkItemID, Source: SourceToPB(m.Source), Text: m.Text, Kind: m.Kind,
		Options: m.Options, InboxId: m.InboxID, FactoryId: m.FactoryID, ThreadRef: m.ThreadRef, IssueState: m.IssueState}
	if m.PullRequest != nil {
		out.PullRequest = PRToPB(m.PullRequest)
	}
	return out
}

// OutboundFromPB converts a pb.OutboundMessage.
func OutboundFromPB(m *pb.OutboundMessage) core.OutboundMessage {
	if m == nil {
		return core.OutboundMessage{}
	}
	return core.OutboundMessage{WorkItemID: m.WorkItemId, Source: SourceFromPB(m.Source), Text: m.Text, Kind: m.Kind,
		Options: m.Options, InboxID: m.InboxId, FactoryID: m.FactoryId, ThreadRef: m.ThreadRef, IssueState: m.IssueState,
		PullRequest: PRFromPB(m.PullRequest)}
}

// HeadersToPB converts http.Header.
func HeadersToPB(h http.Header) []*pb.Header {
	out := make([]*pb.Header, 0, len(h))
	for k, v := range h {
		out = append(out, &pb.Header{Name: k, Values: v})
	}
	return out
}

// HeadersFromPB converts protocol headers to http.Header.
func HeadersFromPB(hs []*pb.Header) http.Header {
	h := http.Header{}
	for _, x := range hs {
		for _, v := range x.Values {
			h.Add(x.Name, v)
		}
	}
	return h
}

// ---------- errors ----------

const errDomain = "openfactory.plugin.v1"

// ToStatus maps a Go error to a gRPC status error. core sentinel errors map to
// their codes and provider HTTP errors (*forgeutil.APIError) carry their status.
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	code := codes.Unknown
	switch {
	case errors.Is(err, core.ErrNotFound):
		code = codes.NotFound
	case errors.Is(err, core.ErrConflict):
		code = codes.AlreadyExists
	case errors.Is(err, core.ErrInvalid):
		code = codes.InvalidArgument
	case errors.Is(err, core.ErrUnauthorized):
		code = codes.Unauthenticated
	case errors.Is(err, core.ErrForbidden):
		code = codes.PermissionDenied
	case errors.Is(err, context.Canceled):
		code = codes.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = codes.DeadlineExceeded
	case errors.Is(err, core.ErrOperationNotSupported):
		code = codes.Unimplemented
	}
	var ae *forgeutil.APIError
	if errors.As(err, &ae) {
		switch {
		case code != codes.Unknown:
		case ae.Status == http.StatusNotFound:
			code = codes.NotFound
		case ae.Status == http.StatusUnauthorized:
			code = codes.Unauthenticated
		case ae.Status == http.StatusForbidden:
			code = codes.PermissionDenied
		case ae.Status == http.StatusConflict:
			code = codes.AlreadyExists
		case ae.Status == http.StatusTooManyRequests:
			code = codes.ResourceExhausted
		case ae.Status >= 500:
			code = codes.Unavailable
		case ae.Status >= 400:
			code = codes.InvalidArgument
		}
		st := status.New(code, err.Error())
		if d, derr := st.WithDetails(&errdetails.ErrorInfo{Reason: "HTTP_ERROR", Domain: errDomain,
			Metadata: map[string]string{"http_status": strconv.Itoa(ae.Status), "method": ae.Method}}); derr == nil {
			st = d
		}
		return st.Err()
	}
	return status.Error(code, err.Error())
}

// RemoteError is an error returned by a plugin.
type RemoteError struct {
	Plugin string
	Code   codes.Code
	Msg    string
}

func (e *RemoteError) Error() string {
	return "plugin " + e.Plugin + ": " + e.Msg
}

// Unwrap maps the gRPC code back to core sentinel errors.
func (e *RemoteError) Unwrap() error {
	switch e.Code {
	case codes.NotFound:
		return core.ErrNotFound
	case codes.AlreadyExists:
		return core.ErrConflict
	case codes.InvalidArgument:
		return core.ErrInvalid
	case codes.Unauthenticated:
		return core.ErrUnauthorized
	case codes.PermissionDenied:
		return core.ErrForbidden
	case codes.Canceled:
		return context.Canceled
	case codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.Unimplemented:
		return core.ErrOperationNotSupported
	}
	return nil
}

// Temporary reports whether the call may succeed when retried.
func (e *RemoteError) Temporary() bool {
	return e.Code == codes.Unavailable || e.Code == codes.ResourceExhausted || e.Code == codes.Aborted
}

// FromStatus maps a gRPC error from plugin back to a Go error. Provider HTTP
// errors come back as *forgeutil.APIError wrapping a *RemoteError.
func FromStatus(plugin string, err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	re := &RemoteError{Plugin: plugin, Code: st.Code(), Msg: st.Message()}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.Domain == errDomain && info.Reason == "HTTP_ERROR" {
			if n, err := strconv.Atoi(info.Metadata["http_status"]); err == nil {
				return &httpError{APIError: forgeutil.APIError{Method: info.Metadata["method"], Status: n, Body: st.Message()}, remote: re}
			}
		}
	}
	return re
}

// httpError is a provider HTTP error relayed by a plugin. errors.As finds the
// *forgeutil.APIError (so forgeutil.StatusCode works) and errors.Is finds the
// core sentinel of the gRPC code.
type httpError struct {
	forgeutil.APIError
	remote *RemoteError
}

func (e *httpError) Error() string { return e.remote.Error() }

func (e *httpError) Unwrap() []error { return []error{&e.APIError, e.remote} }

// Temporary reports whether the call may succeed when retried.
func (e *httpError) Temporary() bool { return e.APIError.Temporary() }
