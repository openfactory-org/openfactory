package webhooks

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/triggers"
)

// MaxWebhooksPerFactory is the documented per-factory webhook limit.
const MaxWebhooksPerFactory = 20

// MaxDeliveryIDHeaderLen bounds deliveryIdHeader.
const MaxDeliveryIDHeaderLen = 64

// IngressURL returns the URL senders post to. For url_token webhooks the
// token becomes the last path segment; pass "" to render a "{token}"
// placeholder (the secret is shown only once).
func IngressURL(publicURL string, w core.Webhook, urlToken string) string {
	u := strings.TrimRight(publicURL, "/") + "/webhooks/w/" + url.PathEscape(w.ID)
	if w.AuthMode == AuthURLToken {
		if urlToken == "" {
			return u + "/{token}"
		}
		return u + "/" + url.PathEscape(urlToken)
	}
	return u
}

// ValidURLToken reports whether s can be used as a url_token secret: only
// letters, digits, "-", ".", "_" and "~".
func ValidURLToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// ValidateConfig checks a webhook resource: auth mode, signature scheme,
// secret name and delivery ID header rules.
func ValidateConfig(w core.Webhook) error {
	var errs []string
	mode := w.AuthMode
	if mode == "" {
		mode = AuthToken
	}
	switch mode {
	case AuthToken, AuthURLToken:
		if w.SignatureScheme != "" {
			errs = append(errs, "signatureScheme is only valid with authMode signature")
		}
	case AuthSignature:
		if w.SignatureScheme == "" {
			errs = append(errs, "signatureScheme is required for authMode signature")
		} else if _, ok := schemes[w.SignatureScheme]; !ok {
			errs = append(errs, fmt.Sprintf("unknown signatureScheme %q (allowed: %s)", w.SignatureScheme, strings.Join(SignatureSchemes(), ", ")))
		}
	default:
		errs = append(errs, fmt.Sprintf("unknown authMode %q (allowed: token, url_token, signature)", w.AuthMode))
	}
	if strings.TrimSpace(w.SecretName) == "" {
		errs = append(errs, "secretName is required")
	}
	if h := w.DeliveryIDHeader; h != "" {
		switch {
		case len(h) > MaxDeliveryIDHeaderLen:
			errs = append(errs, fmt.Sprintf("deliveryIdHeader must be at most %d characters", MaxDeliveryIDHeaderLen))
		case !validHeaderName(h):
			errs = append(errs, fmt.Sprintf("deliveryIdHeader %q is not a valid HTTP header name", h))
		case isCredentialHeader(h):
			errs = append(errs, fmt.Sprintf("deliveryIdHeader %q carries credentials and cannot be used", h))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", core.ErrInvalid, strings.Join(errs, "; "))
	}
	return nil
}

// GenerateSecret creates a secret for a new webhook. Bearer and URL token
// webhooks get a URL-safe "whsec_" token; Standard Webhooks get a
// "whsec_<base64>" signing key. Other signature schemes use the secret the
// provider issues, so an error is returned.
func GenerateSecret(authMode, scheme string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	switch authMode {
	case "", AuthToken, AuthURLToken:
		return "whsec_" + base64.RawURLEncoding.EncodeToString(b), nil
	case AuthSignature:
		if scheme == SchemeStandardWebhooks {
			return "whsec_" + base64.StdEncoding.EncodeToString(b[:24]), nil
		}
		return "", fmt.Errorf("the %s provider issues its own signing secret", scheme)
	}
	return "", fmt.Errorf("unknown auth mode %q", authMode)
}

// RedactHeaders flattens request headers for the delivery log, dropping every
// credential-bearing header (Authorization, cookies, provider signatures and
// anything that looks like a token or secret).
func RedactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, vals := range h {
		if isCredentialHeader(name) {
			continue
		}
		out[http.CanonicalHeaderKey(name)] = strings.Join(vals, ", ")
	}
	return out
}

var credentialHeaders = func() map[string]bool {
	m := map[string]bool{
		"Authorization": true, "Proxy-Authorization": true, "Cookie": true, "Set-Cookie": true,
		"X-Api-Key": true, "Api-Key": true,
	}
	for _, sc := range schemes {
		for _, h := range sc.signatureHeaders {
			m[http.CanonicalHeaderKey(h)] = true
		}
	}
	return m
}()

func isCredentialHeader(name string) bool {
	if credentialHeaders[http.CanonicalHeaderKey(name)] {
		return true
	}
	l := strings.ToLower(name)
	for _, frag := range []string{"signature", "secret", "token", "password", "api-key", "apikey", "credential", "authorization"} {
		if strings.Contains(l, frag) {
			return true
		}
	}
	return false
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// FilterTestResult is the outcome of TestFilter ("Would trigger" /
// "Would not trigger" with the webhook and payload checks).
type FilterTestResult struct {
	WouldTrigger   bool   `json:"would_trigger"`
	WebhookMatched bool   `json:"webhook_matched"`
	PayloadMatched bool   `json:"payload_matched"`
	Reason         string `json:"reason,omitempty"`
}

// TestFilter evaluates a webhook trigger filter ({"webhook_ids": [...],
// "payload": {...}}) against a stored delivery. An invalid filter returns an
// error. Only accepted deliveries carry a payload; others never trigger.
func TestFilter(filter json.RawMessage, d core.WebhookDelivery) (FilterTestResult, error) {
	t := core.Trigger{Provider: triggers.ProviderWebhook, Event: triggers.EventWebhookReceived, Filter: filter}
	if err := triggers.ValidateTrigger(t); err != nil {
		return FilterTestResult{}, err
	}
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(filter, &parts); err != nil {
		return FilterTestResult{}, err
	}
	pattern := parts["payload"]
	delete(parts, "payload")
	idOnly, _ := json.Marshal(parts)

	res := FilterTestResult{}
	ev := &core.Event{
		Provider:   triggers.ProviderWebhook,
		Type:       triggers.EventWebhookReceived,
		Attributes: map[string]any{triggers.AttrWebhookID: d.WebhookID},
		Payload:    d.Body,
	}
	ok, err := triggers.MatchTrigger(ev, core.Trigger{Provider: t.Provider, Event: t.Event, Filter: idOnly})
	if err != nil {
		return FilterTestResult{}, err
	}
	res.WebhookMatched = ok

	var reasons []string
	if !ok {
		reasons = append(reasons, "the delivery came from a different webhook")
	}
	if d.Status != StatusAccepted || len(d.Body) == 0 {
		reasons = append(reasons, "the delivery was not accepted, so it has no stored payload")
	} else {
		pm, err := triggers.MatchPayload(pattern, d.Body)
		switch {
		case err != nil:
			reasons = append(reasons, "the payload filter could not be evaluated: "+err.Error())
		case !pm:
			reasons = append(reasons, "the payload does not match the filter")
		}
		res.PayloadMatched = pm && err == nil
	}
	res.WouldTrigger = res.WebhookMatched && res.PayloadMatched
	sort.Strings(reasons)
	res.Reason = strings.Join(reasons, "; ")
	return res, nil
}
