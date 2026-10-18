package webhooks

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G505: a sender's signature scheme is HMAC-SHA1; verification only
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Signature scheme names (core.Webhook.SignatureScheme).
const (
	SchemeGitHub           = "github"
	SchemePagerDuty        = "pagerduty"
	SchemeSentry           = "sentry"
	SchemeStandardWebhooks = "standard_webhooks"
	SchemeStripe           = "stripe"
	SchemeVercel           = "vercel"
)

// DefaultSignatureTolerance is the maximum age (and clock skew) accepted for
// timestamped signatures (Stripe, Standard Webhooks).
const DefaultSignatureTolerance = 5 * time.Minute

// ErrSignature is returned (wrapped) for every signature verification failure.
var ErrSignature = errors.New("signature verification failed")

func sigErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSignature, fmt.Sprintf(format, args...))
}

type scheme struct {
	// signatureHeaders carry credentials: stripped from the delivery log and
	// not allowed as deliveryIdHeader.
	signatureHeaders []string
	// deliveryHeaders identify a delivery (first non-empty wins).
	deliveryHeaders []string
	verify          func(secret []byte, h http.Header, body []byte, now time.Time, tol time.Duration) error
	sign            func(secret []byte, body []byte, now time.Time, id string) http.Header
}

var schemes = map[string]scheme{
	SchemeGitHub: {
		signatureHeaders: []string{"X-Hub-Signature-256", "X-Hub-Signature"},
		deliveryHeaders:  []string{"X-GitHub-Delivery"},
		verify: func(secret []byte, h http.Header, body []byte, _ time.Time, _ time.Duration) error {
			v := strings.TrimSpace(h.Get("X-Hub-Signature-256"))
			if v == "" {
				return sigErr("missing X-Hub-Signature-256 header")
			}
			hexSig, ok := strings.CutPrefix(v, "sha256=")
			if !ok {
				return sigErr("X-Hub-Signature-256 must start with sha256=")
			}
			if !equalHex(hexSig, hmacSum(sha256.New, secret, body)) {
				return sigErr("signature mismatch")
			}
			return nil
		},
		sign: func(secret, body []byte, _ time.Time, id string) http.Header {
			h := http.Header{}
			h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(hmacSum(sha256.New, secret, body)))
			if id != "" {
				h.Set("X-GitHub-Delivery", id)
			}
			return h
		},
	},
	SchemePagerDuty: {
		signatureHeaders: []string{"X-PagerDuty-Signature"},
		deliveryHeaders:  []string{"X-Webhook-Id"},
		verify: func(secret []byte, h http.Header, body []byte, _ time.Time, _ time.Duration) error {
			v := strings.TrimSpace(h.Get("X-PagerDuty-Signature"))
			if v == "" {
				return sigErr("missing X-PagerDuty-Signature header")
			}
			want := hmacSum(sha256.New, secret, body)
			found := false
			// Several comma-separated v1= signatures are sent during secret rotation.
			for _, part := range strings.Split(v, ",") {
				sig, ok := strings.CutPrefix(strings.TrimSpace(part), "v1=")
				if !ok {
					continue
				}
				found = true
				if equalHex(sig, want) {
					return nil
				}
			}
			if !found {
				return sigErr("X-PagerDuty-Signature has no v1 signature")
			}
			return sigErr("signature mismatch")
		},
		sign: func(secret, body []byte, _ time.Time, id string) http.Header {
			h := http.Header{}
			h.Set("X-PagerDuty-Signature", "v1="+hex.EncodeToString(hmacSum(sha256.New, secret, body)))
			if id != "" {
				h.Set("X-Webhook-Id", id)
			}
			return h
		},
	},
	SchemeSentry: {
		signatureHeaders: []string{"Sentry-Hook-Signature"},
		deliveryHeaders:  []string{"Request-ID"},
		verify: func(secret []byte, h http.Header, body []byte, _ time.Time, _ time.Duration) error {
			v := strings.TrimSpace(h.Get("Sentry-Hook-Signature"))
			if v == "" {
				return sigErr("missing Sentry-Hook-Signature header")
			}
			if !equalHex(v, hmacSum(sha256.New, secret, body)) {
				return sigErr("signature mismatch")
			}
			return nil
		},
		sign: func(secret, body []byte, _ time.Time, id string) http.Header {
			h := http.Header{}
			h.Set("Sentry-Hook-Signature", hex.EncodeToString(hmacSum(sha256.New, secret, body)))
			if id != "" {
				h.Set("Request-ID", id)
			}
			return h
		},
	},
	SchemeStandardWebhooks: {
		signatureHeaders: []string{"Webhook-Signature", "Svix-Signature"},
		deliveryHeaders:  []string{"Webhook-Id", "Svix-Id"},
		verify:           verifyStandardWebhooks,
		sign: func(secret, body []byte, now time.Time, id string) http.Header {
			if id == "" {
				id = "msg_" + strconv.FormatInt(now.UnixNano(), 36)
			}
			ts := strconv.FormatInt(now.Unix(), 10)
			sig := hmacSum(sha256.New, standardWebhooksKey(secret), []byte(id+"."+ts+"."+string(body)))
			h := http.Header{}
			h.Set("Webhook-Id", id)
			h.Set("Webhook-Timestamp", ts)
			h.Set("Webhook-Signature", "v1,"+base64.StdEncoding.EncodeToString(sig))
			return h
		},
	},
	SchemeStripe: {
		signatureHeaders: []string{"Stripe-Signature"},
		verify:           verifyStripe,
		sign: func(secret, body []byte, now time.Time, _ string) http.Header {
			ts := strconv.FormatInt(now.Unix(), 10)
			sig := hmacSum(sha256.New, secret, []byte(ts+"."+string(body)))
			h := http.Header{}
			h.Set("Stripe-Signature", "t="+ts+",v1="+hex.EncodeToString(sig))
			return h
		},
	},
	SchemeVercel: {
		signatureHeaders: []string{"X-Vercel-Signature"},
		verify: func(secret []byte, h http.Header, body []byte, _ time.Time, _ time.Duration) error {
			v := strings.TrimSpace(h.Get("X-Vercel-Signature"))
			if v == "" {
				return sigErr("missing x-vercel-signature header")
			}
			if !equalHex(v, hmacSum(sha1.New, secret, body)) {
				return sigErr("signature mismatch")
			}
			return nil
		},
		sign: func(secret, body []byte, _ time.Time, _ string) http.Header {
			h := http.Header{}
			h.Set("X-Vercel-Signature", hex.EncodeToString(hmacSum(sha1.New, secret, body)))
			return h
		},
	},
}

// SignatureSchemes returns the supported scheme names, sorted.
func SignatureSchemes() []string {
	out := make([]string, 0, len(schemes))
	for k := range schemes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Verify checks a delivery's signature with the given scheme. tolerance <= 0
// uses DefaultSignatureTolerance. All comparisons are constant-time.
func Verify(schemeName string, secret []byte, h http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	sc, ok := schemes[schemeName]
	if !ok {
		return sigErr("unknown signature scheme %q", schemeName)
	}
	if len(secret) == 0 {
		return sigErr("no signing secret configured")
	}
	if tolerance <= 0 {
		tolerance = DefaultSignatureTolerance
	}
	return sc.verify(secret, h, body, now, tolerance)
}

// Sign produces the headers a sender using the scheme would attach (useful
// for CLI test deliveries). id is the delivery ID where the scheme carries one.
func Sign(schemeName string, secret, body []byte, now time.Time, id string) (http.Header, error) {
	sc, ok := schemes[schemeName]
	if !ok {
		return nil, fmt.Errorf("unknown signature scheme %q", schemeName)
	}
	return sc.sign(secret, body, now, id), nil
}

func verifyStandardWebhooks(secret []byte, h http.Header, body []byte, now time.Time, tol time.Duration) error {
	get := func(name string) string {
		if v := strings.TrimSpace(h.Get("Webhook-" + name)); v != "" {
			return v
		}
		return strings.TrimSpace(h.Get("Svix-" + name))
	}
	id, tsStr, sigs := get("Id"), get("Timestamp"), get("Signature")
	if id == "" || tsStr == "" || sigs == "" {
		return sigErr("missing webhook-id, webhook-timestamp or webhook-signature header")
	}
	if err := checkTimestamp(tsStr, now, tol); err != nil {
		return err
	}
	want := hmacSum(sha256.New, standardWebhooksKey(secret), []byte(id+"."+tsStr+"."+string(body)))
	found := false
	for _, part := range strings.Fields(sigs) {
		sig, ok := strings.CutPrefix(part, "v1,")
		if !ok {
			continue // unknown versions (e.g. v1a asymmetric) are ignored
		}
		found = true
		got, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return nil
		}
	}
	if !found {
		return sigErr("webhook-signature has no v1 signature")
	}
	return sigErr("signature mismatch")
}

// standardWebhooksKey decodes a "whsec_<base64>" secret. Secrets that are not
// valid base64 are used as raw bytes.
func standardWebhooksKey(secret []byte) []byte {
	s := strings.TrimSpace(string(secret))
	s = strings.TrimPrefix(s, "whsec_")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil && len(k) > 0 {
			return k
		}
	}
	return []byte(s)
}

func verifyStripe(secret []byte, h http.Header, body []byte, now time.Time, tol time.Duration) error {
	v := strings.TrimSpace(h.Get("Stripe-Signature"))
	if v == "" {
		return sigErr("missing Stripe-Signature header")
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = val
		case "v1":
			sigs = append(sigs, val)
		}
	}
	if ts == "" {
		return sigErr("Stripe-Signature has no timestamp")
	}
	if len(sigs) == 0 {
		return sigErr("Stripe-Signature has no v1 signature")
	}
	if err := checkTimestamp(ts, now, tol); err != nil {
		return err
	}
	want := hmacSum(sha256.New, secret, []byte(ts+"."+string(body)))
	for _, s := range sigs {
		if equalHex(s, want) {
			return nil
		}
	}
	return sigErr("signature mismatch")
}

func checkTimestamp(ts string, now time.Time, tol time.Duration) error {
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return sigErr("invalid timestamp %q", ts)
	}
	d := now.Sub(time.Unix(sec, 0))
	if d > tol {
		return sigErr("timestamp is older than %s", tol)
	}
	if d < -tol {
		return sigErr("timestamp is more than %s in the future", tol)
	}
	return nil
}

func hmacSum(h func() hash.Hash, key, msg []byte) []byte {
	m := hmac.New(h, key)
	m.Write(msg)
	return m.Sum(nil)
}

// equalHex compares a hex-encoded signature with raw MAC bytes in constant
// time (case-insensitive hex).
func equalHex(sigHex string, want []byte) bool {
	got, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(sigHex)))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}
