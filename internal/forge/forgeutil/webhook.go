package forgeutil

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// MaxWebhookBody bounds inbound webhook payloads.
const MaxWebhookBody = 10 << 20

// WriteError writes the standard JSON error envelope.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ReadBody reads at most MaxWebhookBody bytes of a request body.
func ReadBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxWebhookBody {
		return nil, errors.New("request body too large")
	}
	return b, nil
}

// HMACSHA256Hex returns hex(HMAC-SHA256(secret, data)).
func HMACSHA256Hex(secret, data []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyHMACSHA256Hex checks a hex HMAC-SHA256 signature (an optional "sha256=" prefix is stripped).
func VerifyHMACSHA256Hex(secret, body []byte, sig string) bool {
	if len(secret) == 0 {
		return false
	}
	sig = strings.TrimSpace(sig)
	sig = strings.TrimPrefix(sig, "sha256=")
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(m.Sum(nil), got)
}

// ConstantTimeEqual compares two secrets in constant time.
func ConstantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Publish publishes each event on the inbound topic.
func Publish(ctx context.Context, bus core.EventBus, evs []*core.Event) {
	if bus == nil {
		return
	}
	for _, ev := range evs {
		bus.Publish(ctx, core.TopicInboundEvent, ev)
	}
}

// Dedupe remembers recently seen delivery IDs to drop provider retries.
type Dedupe struct {
	mu   sync.Mutex
	ttl  time.Duration
	seen map[string]time.Time
}

// NewDedupe returns a dedupe set with the given retention.
func NewDedupe(ttl time.Duration) *Dedupe {
	return &Dedupe{ttl: ttl, seen: map[string]time.Time{}}
}

// Seen records id and reports whether it was already recorded within the TTL.
func (d *Dedupe) Seen(id string) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if len(d.seen) > 10000 {
		for k, t := range d.seen {
			if now.Sub(t) > d.ttl {
				delete(d.seen, k)
			}
		}
	}
	if t, ok := d.seen[id]; ok && now.Sub(t) <= d.ttl {
		return true
	}
	d.seen[id] = now
	return false
}

// FindIntegrations returns every integration of provider (across all teams) accepted by match.
func FindIntegrations(ctx context.Context, store core.Store, provider string, match func(core.Integration) bool) ([]core.Integration, error) {
	teams, err := store.ListTeams(ctx)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}
	var out []core.Integration
	for _, t := range teams {
		ints, err := store.ListIntegrations(ctx, t.ID)
		if err != nil {
			return nil, fmt.Errorf("list integrations for team %s: %w", t.ID, err)
		}
		for _, in := range ints {
			if !strings.EqualFold(in.Provider, provider) {
				continue
			}
			if match == nil || match(in) {
				out = append(out, in)
			}
		}
	}
	return out, nil
}

// TeamIntegrations returns the team's integrations of provider.
func TeamIntegrations(ctx context.Context, store core.Store, teamID, provider string) ([]core.Integration, error) {
	ints, err := store.ListIntegrations(ctx, teamID)
	if err != nil {
		return nil, err
	}
	var out []core.Integration
	for _, in := range ints {
		if strings.EqualFold(in.Provider, provider) {
			out = append(out, in)
		}
	}
	return out, nil
}

// IntegrationSecret loads the secret named by in.SecretRef (or by in.Config[configKey] when configKey != "").
func IntegrationSecret(ctx context.Context, secrets core.SecretStore, in core.Integration, configKey string) ([]byte, error) {
	name := in.SecretRef
	if configKey != "" {
		name = in.Config[configKey]
	}
	if name == "" {
		return nil, fmt.Errorf("integration %s (%s): no secret configured: %w", in.ID, in.Provider, core.ErrNotFound)
	}
	if secrets == nil {
		return nil, fmt.Errorf("integration %s: no secret store configured", in.ID)
	}
	v, err := secrets.Get(ctx, in.TeamID, "", name)
	if err != nil {
		return nil, fmt.Errorf("integration %s: secret %s: %w", in.ID, name, err)
	}
	return v, nil
}

// LinkActor fills Actor.UserID from a linked external identity, if any.
func LinkActor(ctx context.Context, store core.Store, a *core.Actor) {
	if store == nil || a == nil || a.ExternalID == "" {
		return
	}
	x, err := store.FindExternalIdentity(ctx, a.Provider, a.ExternalID)
	if err == nil && x != nil {
		a.UserID = x.UserID
	} else if err != nil && !errors.Is(err, core.ErrNotFound) {
		slog.Warn("lookup external identity", "provider", a.Provider, "err", err)
	}
}

// Logger returns l or slog.Default.
func Logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}

// SetIntegrationStatus sets the status of an integration record. It returns
// true when the status changed. An unknown record is not an error.
func SetIntegrationStatus(ctx context.Context, store core.Store, id, status, reason string, now time.Time) (bool, error) {
	in, err := store.GetIntegration(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if in.EffectiveStatus() == status && in.StatusReason == reason {
		return false, nil
	}
	in.Status = status
	in.StatusReason = reason
	t := now.UTC()
	in.StatusUpdatedAt = &t
	return true, store.UpsertIntegration(ctx, in)
}
