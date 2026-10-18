package webhooks

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// Known-good vectors computed independently (Python hmac). The GitHub vector
// is GitHub's documented example; the Standard Webhooks vector is the
// specification's reference example.
func TestVerifyKnownVectors(t *testing.T) {
	const (
		ghBody = "Hello, World!"
		pdBody = `{"event":{"id":"01","event_type":"incident.triggered"}}`
		snBody = `{"action":"created","data":{"issue":{"level":"fatal"}}}`
		swBody = `{"test": 2432232314}`
		stBody = `{"id":"evt_1","type":"payment_intent.succeeded"}`
		vcBody = `{"type":"deployment.succeeded","payload":{"deployment":{"id":"dpl_1"}}}`
	)
	swNow := time.Unix(1614265330, 0).Add(30 * time.Second)
	stNow := time.Unix(1700000000, 0).Add(time.Minute)

	cases := []struct {
		name   string
		scheme string
		secret string
		h      http.Header
		body   string
		now    time.Time
		ok     bool
	}{
		{"github ok", SchemeGitHub, "It's a Secret to Everybody", hdr("X-Hub-Signature-256", "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"), ghBody, time.Now(), true},
		{"github uppercase hex ok", SchemeGitHub, "It's a Secret to Everybody", hdr("X-Hub-Signature-256", "sha256=757107EA0EB2509FC211221CCE984B8A37570B6D7586C22C46F4379C8B043E17"), ghBody, time.Now(), true},
		{"github tampered body", SchemeGitHub, "It's a Secret to Everybody", hdr("X-Hub-Signature-256", "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"), ghBody + " ", time.Now(), false},
		{"github wrong secret", SchemeGitHub, "other", hdr("X-Hub-Signature-256", "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"), ghBody, time.Now(), false},
		{"github missing prefix", SchemeGitHub, "It's a Secret to Everybody", hdr("X-Hub-Signature-256", "757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"), ghBody, time.Now(), false},
		{"github missing header", SchemeGitHub, "s", hdr(), ghBody, time.Now(), false},
		{"github sha1 header alone rejected", SchemeGitHub, "s", hdr("X-Hub-Signature", "sha1=00"), ghBody, time.Now(), false},

		{"pagerduty ok", SchemePagerDuty, "pd-secret", hdr("X-PagerDuty-Signature", "v1=7f0bba8706ef6d18b71140ba988586b9fd67c7356eadd44d139b833642dc4750"), pdBody, time.Now(), true},
		{"pagerduty rotation list", SchemePagerDuty, "pd-secret", hdr("X-PagerDuty-Signature", "v1=deadbeef, v1=7f0bba8706ef6d18b71140ba988586b9fd67c7356eadd44d139b833642dc4750"), pdBody, time.Now(), true},
		{"pagerduty no v1", SchemePagerDuty, "pd-secret", hdr("X-PagerDuty-Signature", "7f0bba8706ef6d18b71140ba988586b9fd67c7356eadd44d139b833642dc4750"), pdBody, time.Now(), false},
		{"pagerduty mismatch", SchemePagerDuty, "pd-secret", hdr("X-PagerDuty-Signature", "v1=7f0bba8706ef6d18b71140ba988586b9fd67c7356eadd44d139b833642dc4751"), pdBody, time.Now(), false},

		{"sentry ok", SchemeSentry, "sentry-client-secret", hdr("Sentry-Hook-Signature", "90d2c2e3bd31952f748093750d5c60166e7e29c0120104ad4dcabeae09f3e76d"), snBody, time.Now(), true},
		{"sentry mismatch", SchemeSentry, "sentry-client-secret", hdr("Sentry-Hook-Signature", "90d2c2e3bd31952f748093750d5c60166e7e29c0120104ad4dcabeae09f3e76d"), `{"action":"resolved"}`, time.Now(), false},
		{"sentry not hex", SchemeSentry, "sentry-client-secret", hdr("Sentry-Hook-Signature", "zz"), snBody, time.Now(), false},

		// The public test vector of the Standard Webhooks specification, not a real secret.
		{"standard webhooks ok", SchemeStandardWebhooks, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
			hdr("webhook-id", "msg_p5jXN8AQM9LWM0D4loKWxJek", "webhook-timestamp", "1614265330", "webhook-signature", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), swBody, swNow, true},
		{"standard webhooks svix headers", SchemeStandardWebhooks, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
			hdr("svix-id", "msg_p5jXN8AQM9LWM0D4loKWxJek", "svix-timestamp", "1614265330", "svix-signature", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), swBody, swNow, true},
		{"standard webhooks multiple sigs", SchemeStandardWebhooks, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
			hdr("webhook-id", "msg_p5jXN8AQM9LWM0D4loKWxJek", "webhook-timestamp", "1614265330", "webhook-signature", "v1a,abc v1,Zm9v v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), swBody, swNow, true},
		{"standard webhooks expired", SchemeStandardWebhooks, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
			hdr("webhook-id", "msg_p5jXN8AQM9LWM0D4loKWxJek", "webhook-timestamp", "1614265330", "webhook-signature", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), swBody, swNow.Add(5 * time.Minute), false},
		{"standard webhooks future", SchemeStandardWebhooks, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
			hdr("webhook-id", "msg_p5jXN8AQM9LWM0D4loKWxJek", "webhook-timestamp", "1614265330", "webhook-signature", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), swBody, time.Unix(1614265330, 0).Add(-6 * time.Minute), false},
		{"standard webhooks wrong id", SchemeStandardWebhooks, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
			hdr("webhook-id", "msg_other", "webhook-timestamp", "1614265330", "webhook-signature", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), swBody, swNow, false},
		{"standard webhooks missing headers", SchemeStandardWebhooks, "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
			hdr("webhook-signature", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), swBody, swNow, false},

		{"stripe ok", SchemeStripe, "whsec_stripetest", hdr("Stripe-Signature", "t=1700000000,v1=0cf1c7e67157312215394468ff01f7a1601cdcaad20a5ba22950f6e598f7978c"), stBody, stNow, true},
		{"stripe with v0 and extra v1", SchemeStripe, "whsec_stripetest", hdr("Stripe-Signature", "t=1700000000,v1=00,v0=abc,v1=0cf1c7e67157312215394468ff01f7a1601cdcaad20a5ba22950f6e598f7978c"), stBody, stNow, true},
		{"stripe expired", SchemeStripe, "whsec_stripetest", hdr("Stripe-Signature", "t=1700000000,v1=0cf1c7e67157312215394468ff01f7a1601cdcaad20a5ba22950f6e598f7978c"), stBody, time.Unix(1700000000, 0).Add(5*time.Minute + time.Second), false},
		{"stripe timestamp changed", SchemeStripe, "whsec_stripetest", hdr("Stripe-Signature", "t=1700000001,v1=0cf1c7e67157312215394468ff01f7a1601cdcaad20a5ba22950f6e598f7978c"), stBody, stNow, false},
		{"stripe no timestamp", SchemeStripe, "whsec_stripetest", hdr("Stripe-Signature", "v1=0cf1c7e67157312215394468ff01f7a1601cdcaad20a5ba22950f6e598f7978c"), stBody, stNow, false},
		{"stripe no v1", SchemeStripe, "whsec_stripetest", hdr("Stripe-Signature", "t=1700000000,v0=abc"), stBody, stNow, false},

		{"vercel ok", SchemeVercel, "vercel-secret", hdr("x-vercel-signature", "7acd5529b996834892bea6c8d04de0e94f5d4909"), vcBody, time.Now(), true},
		{"vercel mismatch", SchemeVercel, "vercel-secret-2", hdr("x-vercel-signature", "7acd5529b996834892bea6c8d04de0e94f5d4909"), vcBody, time.Now(), false},
		{"vercel missing", SchemeVercel, "vercel-secret", hdr(), vcBody, time.Now(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.scheme, []byte(tc.secret), tc.h, []byte(tc.body), tc.now, 0)
			if tc.ok && err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("Verify succeeded, want failure")
				}
				if !errors.Is(err, ErrSignature) {
					t.Fatalf("error %v does not wrap ErrSignature", err)
				}
			}
		})
	}
}

func TestVerifyErrors(t *testing.T) {
	if err := Verify("bogus", []byte("s"), hdr(), nil, time.Now(), 0); err == nil {
		t.Fatal("unknown scheme accepted")
	}
	if err := Verify(SchemeGitHub, nil, hdr(), nil, time.Now(), 0); err == nil {
		t.Fatal("empty secret accepted")
	}
}

func TestSignRoundTrip(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	now := time.Unix(1750000000, 0)
	for _, sc := range SignatureSchemes() {
		secret := []byte("round-trip-secret")
		if sc == SchemeStandardWebhooks {
			s, err := GenerateSecret(AuthSignature, sc)
			if err != nil {
				t.Fatal(err)
			}
			secret = []byte(s)
		}
		h, err := Sign(sc, secret, body, now, "dlv_1")
		if err != nil {
			t.Fatal(err)
		}
		if err := Verify(sc, secret, h, body, now.Add(time.Minute), 0); err != nil {
			t.Errorf("%s: round trip failed: %v", sc, err)
		}
		if err := Verify(sc, secret, h, append(body, ' '), now, 0); err == nil {
			t.Errorf("%s: tampered body accepted", sc)
		}
	}
	if _, err := Sign("nope", nil, nil, now, ""); err == nil {
		t.Fatal("unknown scheme signed")
	}
}
