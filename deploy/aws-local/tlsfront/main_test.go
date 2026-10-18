package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestUnsignedHost(t *testing.T) {
	const pub = "aws.k8s.orb.local:9443"
	for _, tc := range []struct {
		name, method, target, ctype, auth, want string
	}{
		{"signed S3", "PUT", "/bkt/k", "", "AWS4-HMAC-SHA256 Credential=x", pub},
		{"signed STS", "POST", "/", "application/x-www-form-urlencoded", "AWS4-HMAC-SHA256 Credential=x", pub},
		{"presigned GET", "GET", "/bkt/k?X-Amz-Signature=ab&X-Amz-Credential=c", "", "", pub},
		{"health", "GET", "/__admin/health", "", "", pub},
		{"pod identity broker", "GET", "/__pod-identity/v1/credentials", "", "", pub},
		{"unsigned web identity", "POST", "/", "application/x-www-form-urlencoded; charset=utf-8", "", "sts.amazonaws.com"},
		{"anonymous read", "GET", "/bkt/public.txt", "", "", "s3.amazonaws.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "https://"+pub+tc.target, strings.NewReader(""))
			if tc.ctype != "" {
				r.Header.Set("Content-Type", tc.ctype)
			}
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			if got := unsignedHost(r, "sts.amazonaws.com", "s3.amazonaws.com"); got != tc.want {
				t.Fatalf("host = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRoutePodIdentity(t *testing.T) {
	var gotPath, gotQuery, gotAuth, gotHost string
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotAuth, gotHost = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Host
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"code":"AccessDenied"}`)
	}))
	defer broker.Close()
	bu, _ := url.Parse(broker.URL)
	emulator := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "emulator "+r.URL.Path)
	})
	h := route(emulator, newBrokerProxy(bu))

	get := func(target, auth string) (int, string) {
		r := httptest.NewRequest("GET", "https://aws.k8s.orb.local:9443"+target, nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}

	code, body := get("/__pod-identity/v1/credentials?x=1", "raw-token")
	if code != http.StatusForbidden || body != `{"code":"AccessDenied"}` {
		t.Fatalf("broker answer: %d %q", code, body)
	}
	if gotPath != "/v1/credentials" || gotQuery != "x=1" || gotAuth != "raw-token" || gotHost != bu.Host {
		t.Fatalf("broker saw path=%q query=%q auth=%q host=%q", gotPath, gotQuery, gotAuth, gotHost)
	}
	// Everything else still goes to the emulator.
	for _, p := range []string{"/", "/bkt/key", "/__admin/health", "/__pod-identity", "/clusters/of-local"} {
		if code, body := get(p, ""); code != 200 || body != "emulator "+p {
			t.Errorf("%s: %d %q", p, code, body)
		}
	}
}
