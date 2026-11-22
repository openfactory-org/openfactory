package identity

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
)

func TestNewKeyAndLoad(t *testing.T) {
	dir := t.TempDir()
	k, err := NewKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(KeyPath(dir))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v, %v", fi.Mode(), err)
	}
	k2, err := NewKey(dir)
	if err != nil || !k.Equal(k2) {
		t.Fatal("a second NewKey must keep the key")
	}
	if _, err := Load(dir); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("load before enroll: %v", err)
	}
	if err := Save(dir, Identity{WorkerID: "wkr_1", ControlURL: "https://cp.test", EnrolledAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	id, err := Load(dir)
	if err != nil || id.WorkerID != "wkr_1" || !id.Key.Equal(k) {
		t.Fatalf("load = %+v, %v", id, err)
	}
	// The key signs a challenge that the hub verifies.
	ch := direct.AuthChallenge{Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), ConnectionID: "c1"}
	resp, err := direct.SignChallenge(id.Key, id.WorkerID, ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.VerifyChallenge(k.Public().(ed25519.PublicKey), id.WorkerID, ch, resp); err != nil {
		t.Fatal(err)
	}
}

func TestEnroll(t *testing.T) {
	var got EnrollRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EnrollPath || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"worker_id":"wkr_9","control_url":"https://cp.test"}`))
	}))
	defer srv.Close()
	req := EnrollRequest{
		EnrollmentToken: "ofe_test", Name: "build-01", PublicKey: "AAAA",
		Facts: Facts{WorkerFacts: direct.WorkerFacts{Arch: "amd64", VCPUs: 8, KVM: true}, DiskGiB: 100},
		Slots: 2, SlotSize: SlotSize{VCPUs: 2, MemoryMiB: 4096, ScratchGiB: 40},
	}
	out, err := Enroll(context.Background(), srv.Client(), srv.URL, req)
	if err != nil || out.WorkerID != "wkr_9" {
		t.Fatalf("enroll = %+v, %v", out, err)
	}
	if got.EnrollmentToken != "ofe_test" || got.Facts.Arch != "amd64" || got.Facts.DiskGiB != 100 || got.SlotSize.MemoryMiB != 4096 {
		t.Fatalf("request = %+v", got)
	}
	req.Facts.KVM = false
	if _, err := Enroll(context.Background(), srv.Client(), srv.URL, req); !errors.Is(err, rtcore.ErrInvalid) {
		t.Fatalf("no KVM: %v", err)
	}
}

func TestEnrollRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := Enroll(context.Background(), srv.Client(), srv.URL, EnrollRequest{EnrollmentToken: "ofe_x", Facts: Facts{WorkerFacts: direct.WorkerFacts{KVM: true}}})
	if err == nil {
		t.Fatal("want an error")
	}
}
