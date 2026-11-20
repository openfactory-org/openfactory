// Package webhooks implements custom webhook ingress: authentication (bearer
// token, URL token, provider signatures), delivery rules (POST JSON up to
// 256 KB, rate limits, deduplication), the delivery log, and publication of
// accepted deliveries as core.Event values for automation matching.
package webhooks

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Authentication modes (core.Webhook.AuthMode).
const (
	AuthToken     = "token"
	AuthURLToken  = "url_token"
	AuthSignature = "signature"
)

// Delivery statuses (core.WebhookDelivery.Status).
const (
	StatusAccepted     = "accepted"
	StatusRejected     = "rejected"
	StatusDeduplicated = "deduplicated"
)

// Rejection reasons recorded in core.WebhookDelivery.Reason.
const (
	ReasonAuth        = "auth"
	ReasonInvalidJSON = "invalid_json"
	ReasonTooLarge    = "too_large"
	ReasonRateLimited = "rate_limited"
	ReasonDuplicate   = "duplicate"
	ReasonSecret      = "secret_unavailable"
)

// Defaults for Options.
const (
	DefaultMaxBodyBytes     = 256 << 10
	DefaultRateLimit        = 60 // accepted-for-evaluation deliveries per webhook per minute
	DefaultAuthFailureLimit = 60 // failed authentication attempts per webhook per minute
)

// SenderDeliveryIDHeader is the generic delivery ID header a sender may set.
const SenderDeliveryIDHeader = "X-OF-Delivery-Id"

// Options configures a Handler.
type Options struct {
	Logger *slog.Logger
	// Now returns the current time (default time.Now).
	Now func() time.Time
	// MaxBodyBytes caps the request body (default 256 KiB).
	MaxBodyBytes int64
	// RateLimit is the per-webhook deliveries-per-minute budget (default 60;
	// negative disables).
	RateLimit int
	// AuthFailureLimit is the separate per-webhook budget of failed
	// authentication attempts per minute (default 60; negative disables).
	AuthFailureLimit int
	// SignatureTolerance bounds timestamped signatures (default 5 minutes).
	SignatureTolerance time.Duration
	// Dispatch, when set, is called (asynchronously, after the 202 response)
	// with each accepted event instead of publishing it on the bus; the
	// returned run IDs are recorded on the delivery. engine.HandleEvent fits.
	Dispatch func(ctx context.Context, ev *core.Event) ([]*core.Run, error)
}

// Handler serves the webhook ingress endpoints.
type Handler struct {
	store   core.Store
	secrets core.SecretStore
	bus     core.EventBus
	log     *slog.Logger
	now     func() time.Time
	maxBody int64
	tol     time.Duration

	dispatch func(ctx context.Context, ev *core.Event) ([]*core.Run, error)

	rate     *windowLimiter
	authFail *windowLimiter
	locks    [64]sync.Mutex
	wg       sync.WaitGroup
}

// NewHandler returns a webhook ingress handler.
func NewHandler(store core.Store, secrets core.SecretStore, bus core.EventBus, opts Options) *Handler {
	h := &Handler{
		store:    store,
		secrets:  secrets,
		bus:      bus,
		log:      opts.Logger,
		now:      opts.Now,
		maxBody:  opts.MaxBodyBytes,
		tol:      opts.SignatureTolerance,
		dispatch: opts.Dispatch,
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.maxBody <= 0 {
		h.maxBody = DefaultMaxBodyBytes
	}
	if h.tol <= 0 {
		h.tol = DefaultSignatureTolerance
	}
	rl := opts.RateLimit
	if rl == 0 {
		rl = DefaultRateLimit
	}
	al := opts.AuthFailureLimit
	if al == 0 {
		al = DefaultAuthFailureLimit
	}
	h.rate = newSharedWindowLimiter(store, RateLimitKVPrefix, rl, time.Minute, h.log)
	h.authFail = newWindowLimiter(al, time.Minute)
	return h
}

// Register mounts POST /webhooks/w/{uid} (bearer token or provider signature)
// and POST /webhooks/w/{uid}/{token} (URL token).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/w/{uid}", func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, r.PathValue("uid"), "", false)
	})
	mux.HandleFunc("POST /webhooks/w/{uid}/{token}", func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, r.PathValue("uid"), r.PathValue("token"), true)
	})
}

// Wait blocks until background dispatches started by accepted deliveries
// have finished (used in tests and graceful shutdown).
func (h *Handler) Wait() { h.wg.Wait() }

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, uid, urlToken string, viaURLToken bool) {
	ctx := r.Context()
	wh, err := h.store.GetWebhook(ctx, uid)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "webhook not found")
			return
		}
		h.log.Error("webhook lookup failed", "webhook_id", uid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "webhook lookup failed")
		return
	}
	if !wh.Enabled {
		writeError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	now := h.now()
	rec := &core.WebhookDelivery{
		ID:         core.NewID("whd"),
		WebhookID:  wh.ID,
		Headers:    RedactHeaders(r.Header),
		ReceivedAt: now.UTC(),
	}

	if over, retry := h.authFail.exhausted(wh.ID, now); over {
		writeRetryAfter(w, retry, "too many failed authentication attempts")
		return
	}

	body, tooLarge, err := readBody(r.Body, h.maxBody)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "could not read request body")
		return
	}
	if tooLarge {
		h.reject(ctx, rec, ReasonTooLarge)
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "request body exceeds "+strconv.FormatInt(h.maxBody/1024, 10)+" KB")
		return
	}

	teamID := ""
	if f, err := h.store.GetFactory(ctx, wh.FactoryID); err == nil {
		teamID = f.TeamID
	} else if !errors.Is(err, core.ErrNotFound) {
		h.log.Error("webhook factory lookup failed", "webhook_id", wh.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "webhook lookup failed")
		return
	}

	if err := h.authenticate(ctx, teamID, wh, r.Header, body, urlToken, viaURLToken, now); err != nil {
		var se *secretError
		if errors.As(err, &se) {
			h.log.Error("webhook secret unavailable", "webhook_id", wh.ID, "secret", wh.SecretName, "err", se.err)
			h.reject(ctx, rec, ReasonSecret)
			writeError(w, http.StatusInternalServerError, "internal", "webhook secret unavailable")
			return
		}
		h.authFail.add(wh.ID, now)
		h.log.Info("webhook delivery rejected: authentication failed", "webhook_id", wh.ID, "err", err)
		h.reject(ctx, rec, ReasonAuth)
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication failed")
		return
	}

	if ok, retry := h.rate.allow(wh.ID, now); !ok {
		h.reject(ctx, rec, ReasonRateLimited)
		writeRetryAfter(w, retry, "webhook exceeded "+strconv.Itoa(h.rate.limit)+" deliveries per minute")
		return
	}

	if !json.Valid(body) {
		h.reject(ctx, rec, ReasonInvalidJSON)
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}

	identity := DeliveryIdentity(wh, r.Header, body)
	rec.DeliveryID = identity

	lock := h.lockFor(wh.ID + "\x00" + identity)
	lock.Lock()
	existing, err := h.store.FindWebhookDelivery(ctx, wh.ID, identity)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		lock.Unlock()
		h.log.Error("webhook dedupe lookup failed", "webhook_id", wh.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record delivery")
		return
	}
	if err == nil && existing != nil && (existing.Status == StatusAccepted || existing.Status == StatusDeduplicated) {
		rec.Status, rec.Reason = StatusDeduplicated, ReasonDuplicate
		if err := h.store.CreateWebhookDelivery(ctx, rec); err != nil {
			h.log.Warn("recording duplicate delivery failed", "webhook_id", wh.ID, "err", err)
		}
		lock.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]any{"delivery_id": identity, "status": "duplicate"})
		return
	}
	rec.Status = StatusAccepted
	rec.Body = json.RawMessage(body)
	ev := &core.Event{
		ID:         core.NewID("evt"),
		TeamID:     teamID,
		FactoryID:  wh.FactoryID,
		Provider:   "webhook",
		Type:       "received",
		DeliveryID: identity,
		ActionKey:  ActionKey(wh.ID, identity),
		Actor:      core.Actor{Provider: "webhook", ExternalID: wh.ID, Handle: wh.Name},
		Source:     core.SourceRef{Provider: "webhook", Kind: "delivery", ExternalID: identity, Title: payloadTitle(body, wh.Name)},
		Attributes: map[string]any{
			"webhook_id":         wh.ID,
			"webhook_name":       wh.Name,
			"delivery_record_id": rec.ID,
		},
		Payload:    json.RawMessage(body),
		ReceivedAt: now.UTC(),
	}
	// Without a dispatcher the event goes to the bus: record the delivery and
	// enqueue the event in one transaction (when the store supports it), so an
	// accepted delivery always produces its event.
	publish := h.dispatch == nil && h.bus != nil
	err = core.RunInTx(context.WithoutCancel(ctx), h.store, func(txCtx context.Context) error {
		if err := h.store.CreateWebhookDelivery(txCtx, rec); err != nil {
			return err
		}
		if publish {
			h.bus.Publish(txCtx, core.TopicInboundEvent, ev)
		}
		return nil
	})
	if err != nil {
		lock.Unlock()
		h.log.Error("recording delivery failed", "webhook_id", wh.ID, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not record delivery")
		return
	}
	lock.Unlock()

	h.log.Info("webhook delivery accepted", "webhook_id", wh.ID, "delivery_id", identity, "bytes", len(body))
	writeJSON(w, http.StatusAccepted, map[string]any{"delivery_id": identity, "status": "accepted"})
	h.deliver(ctx, ev, rec)
}

// ActionKey is the action key of a custom webhook delivery: the webhook ID
// and the delivery identity.
func ActionKey(webhookID, identity string) string {
	return "webhook:" + webhookID + ":" + identity
}

func (h *Handler) deliver(ctx context.Context, ev *core.Event, rec *core.WebhookDelivery) {
	if h.dispatch == nil {
		return // published together with the delivery record
	}
	bg := context.WithoutCancel(ctx)
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		runs, err := h.dispatch(bg, ev)
		if err != nil {
			h.log.Error("webhook dispatch failed", "webhook_id", rec.WebhookID, "delivery_id", rec.DeliveryID, "err", err)
		}
		if len(runs) == 0 {
			return
		}
		for _, r := range runs {
			if r != nil {
				rec.RunIDs = append(rec.RunIDs, r.ID)
			}
		}
		if err := h.store.UpdateWebhookDelivery(bg, rec); err != nil {
			h.log.Warn("recording delivery run IDs failed", "delivery", rec.ID, "err", err)
		}
	}()
}

func (h *Handler) reject(ctx context.Context, rec *core.WebhookDelivery, reason string) {
	rec.Status, rec.Reason = StatusRejected, reason
	rec.DeliveryID, rec.Body = "", nil // rejected entries record metadata only
	if err := h.store.CreateWebhookDelivery(ctx, rec); err != nil {
		h.log.Warn("recording rejected delivery failed", "webhook_id", rec.WebhookID, "err", err)
	}
}

func (h *Handler) lockFor(key string) *sync.Mutex {
	f := fnv.New32a()
	_, _ = f.Write([]byte(key))
	return &h.locks[f.Sum32()%uint32(len(h.locks))]
}

type secretError struct{ err error }

func (e *secretError) Error() string { return "secret unavailable: " + e.err.Error() }

func (h *Handler) authenticate(ctx context.Context, teamID string, wh *core.Webhook, hdr http.Header, body []byte, urlToken string, viaURLToken bool, now time.Time) error {
	mode := wh.AuthMode
	if mode == "" {
		mode = AuthToken
	}
	// The URL token path is only valid for url_token webhooks, and they are
	// only valid on it.
	if viaURLToken != (mode == AuthURLToken) {
		return errors.New("wrong ingress URL form for auth mode " + mode)
	}
	if h.secrets == nil {
		return &secretError{errors.New("no secret store configured")}
	}
	secret, err := h.secrets.Get(ctx, teamID, wh.FactoryID, wh.SecretName)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return errors.New("webhook secret not found")
		}
		return &secretError{err}
	}
	if len(secret) == 0 {
		return errors.New("webhook secret is empty")
	}
	switch mode {
	case AuthToken:
		const prefix = "bearer "
		v := strings.TrimSpace(hdr.Get("Authorization"))
		if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
			return errors.New("missing bearer token")
		}
		if !constantTimeEqual([]byte(strings.TrimSpace(v[len(prefix):])), secret) {
			return errors.New("bearer token mismatch")
		}
		return nil
	case AuthURLToken:
		if !constantTimeEqual([]byte(urlToken), secret) {
			return errors.New("URL token mismatch")
		}
		return nil
	case AuthSignature:
		return Verify(wh.SignatureScheme, secret, hdr, body, now, h.tol)
	}
	return errors.New("unknown auth mode " + mode)
}

// constantTimeEqual compares secrets without leaking their contents through
// timing (the comparison of SHA-256 digests also hides length differences).
func constantTimeEqual(a, b []byte) bool {
	da, db := sha256.Sum256(a), sha256.Sum256(b)
	return subtle.ConstantTimeCompare(da[:], db[:]) == 1
}

func readBody(r io.Reader, max int64) ([]byte, bool, error) {
	if r == nil {
		return nil, false, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > max {
		return nil, true, nil
	}
	return b, false, nil
}

// payloadTitleKeys are the top-level payload fields that name a delivery,
// in order of preference.
var payloadTitleKeys = []string{"title", "subject", "summary"}

// payloadTitle returns the first non-empty string field of payloadTitleKeys
// in a JSON object body, cut to 200 bytes, or def. The work item of the
// delivery gets this title, so a payload with a title is not named after
// the webhook only.
func payloadTitle(body []byte, def string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return def
	}
	for _, k := range payloadTitleKeys {
		var s string
		if json.Unmarshal(obj[k], &s) != nil {
			continue
		}
		s = strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
		if s == "" {
			continue
		}
		if len(s) > 200 {
			cut := 200
			for cut > 0 && (s[cut]&0xC0) == 0x80 {
				cut--
			}
			s = s[:cut]
		}
		return s
	}
	return def
}

// DeliveryIdentity returns the identity used to deduplicate a delivery, in
// order of preference: the webhook's configured delivery ID header, the
// provider's own delivery header (signature webhooks), the generic
// X-OF-Delivery-Id header, and finally "sha256:<hex>" of the body.
func DeliveryIdentity(wh *core.Webhook, hdr http.Header, body []byte) string {
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 200 {
			s = s[:200]
		}
		return s
	}
	if wh.DeliveryIDHeader != "" {
		if v := clip(hdr.Get(wh.DeliveryIDHeader)); v != "" {
			return v
		}
	}
	if wh.AuthMode == AuthSignature {
		if sc, ok := schemes[wh.SignatureScheme]; ok {
			for _, name := range sc.deliveryHeaders {
				if v := clip(hdr.Get(name)); v != "" {
					return v
				}
			}
		}
	}
	if v := clip(hdr.Get(SenderDeliveryIDHeader)); v != "" {
		return v
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func writeRetryAfter(w http.ResponseWriter, retry time.Duration, msg string) {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "rate_limited", msg)
}
