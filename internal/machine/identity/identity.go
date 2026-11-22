// Package identity creates and loads the Ed25519 key pair of a worker host
// and enrolls the host with the control plane.
package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
)

// File names in the identity directory.
const (
	KeyFile    = "ed25519.key"
	PubFile    = "ed25519.pub"
	WorkerFile = "worker.json"
)

// EnrollPath is the enrollment endpoint of the control plane.
const EnrollPath = "/api/v1/workers/enroll"

// Identity is the enrolled identity of a worker host.
type Identity struct {
	WorkerID   string    `json:"worker_id"`
	ControlURL string    `json:"control_url"`
	EnrolledAt time.Time `json:"enrolled_at"`
	// Key is not stored in worker.json.
	Key ed25519.PrivateKey `json:"-"`
}

// KeyPath returns the private key file in dir.
func KeyPath(dir string) string { return filepath.Join(dir, KeyFile) }

// NewKey makes a key pair and writes it to dir: the private key as a PKCS #8
// PEM block with mode 0600 and the raw public key in base64 with mode 0644.
// An existing key is kept and returned.
func NewKey(dir string) (ed25519.PrivateKey, error) {
	if k, err := direct.LoadWorkerKey(KeyPath(dir)); err == nil {
		return k, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := writeAtomic(KeyPath(dir), pemData, 0o600); err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(dir, PubFile), []byte(PublicKeyString(pub)+"\n"), 0o644); err != nil {
		return nil, err
	}
	return priv, nil
}

// PublicKeyString returns the raw 32-byte public key in standard base64,
// as the enrollment request wants it.
func PublicKeyString(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// Load reads the identity in dir.
func Load(dir string) (Identity, error) {
	var id Identity
	data, err := os.ReadFile(filepath.Join(dir, WorkerFile)) //nolint:gosec // G304: the configured identity directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return id, fmt.Errorf("%w: this host is not enrolled; run worker enroll", rtcore.ErrNotFound)
		}
		return id, err
	}
	if err := json.Unmarshal(data, &id); err != nil {
		return id, fmt.Errorf("parse %s: %w", WorkerFile, err)
	}
	k, err := direct.LoadWorkerKey(KeyPath(dir))
	if err != nil {
		return id, err
	}
	id.Key = k
	return id, nil
}

// Save writes worker.json to dir.
func Save(dir string, id Identity) error {
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, WorkerFile), append(data, '\n'), 0o644)
}

// SlotSize is the slot size of an enrollment request.
type SlotSize struct {
	VCPUs      int `json:"vcpus"`
	MemoryMiB  int `json:"memory_mib"`
	ScratchGiB int `json:"scratch_gib,omitempty"`
}

// Facts are the host facts of an enrollment request.
type Facts struct {
	direct.WorkerFacts
	// DiskGiB is the total disk of the data directory in GiB.
	DiskGiB int64 `json:"disk_gib,omitempty"`
}

// EnrollRequest is the body of POST /api/v1/workers/enroll.
type EnrollRequest struct {
	EnrollmentToken string            `json:"enrollment_token"` //nolint:gosec // G117: the single-use token goes to the control plane on purpose
	Name            string            `json:"name"`
	PublicKey       string            `json:"public_key"`
	Facts           Facts             `json:"facts"`
	Labels          map[string]string `json:"labels,omitempty"`
	Slots           int               `json:"slots"`
	SlotSize        SlotSize          `json:"slot_size"`
}

// EnrollResponse is the answer of the control plane.
type EnrollResponse struct {
	WorkerID   string `json:"worker_id"`
	ControlURL string `json:"control_url"`
}

// Enroll posts req to the control plane at baseURL.
func Enroll(ctx context.Context, hc *http.Client, baseURL string, req EnrollRequest) (EnrollResponse, error) {
	var out EnrollResponse
	if req.EnrollmentToken == "" {
		return out, fmt.Errorf("%w: an enrollment token is required", rtcore.ErrInvalid)
	}
	if !req.Facts.KVM {
		return out, fmt.Errorf("%w: this host has no usable /dev/kvm; a worker needs KVM (run worker doctor)", rtcore.ErrInvalid)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+EnrollPath, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(r)
	if err != nil {
		return out, fmt.Errorf("enroll: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return out, fmt.Errorf("enroll: the control plane answered %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, &out); err != nil || out.WorkerID == "" {
		return out, fmt.Errorf("enroll: invalid answer of the control plane: %s", strings.TrimSpace(string(data)))
	}
	if out.ControlURL == "" {
		out.ControlURL = strings.TrimRight(baseURL, "/")
	}
	return out, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
