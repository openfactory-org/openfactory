// Package mmds is the session document that the worker agent writes to the
// Firecracker microVM metadata service (MMDS, version 2) and that vm-init
// reads once. It also has the guest client.
package mmds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Address is the MMDS address in the guest.
const Address = "169.254.169.254"

// MaxDocument is the largest document the agent writes. A larger task is
// refused with invalid_spec.
const MaxDocument = 60 << 10

// Document is the root of the MMDS data store.
type Document struct {
	OpenFactory Payload `json:"openfactory"`
}

// Payload is the openfactory key of the data store.
type Payload struct {
	V int `json:"v"`
	// Template marks the document of a snapshot template. vm-init stops
	// after hello and waits for go.
	Template bool `json:"template,omitempty"`
	// Cleared marks the data store after vm-init read it.
	Cleared bool `json:"cleared,omitempty"`

	TaskID      string `json:"task_id,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	ClockUnixNs int64  `json:"clock_unix_ns,omitempty"`
	// Entropy is 64 random bytes in base64, new for each VM.
	Entropy string `json:"entropy,omitempty"`
	// MachineID is 32 lower-case hex digits, new for each VM.
	MachineID string            `json:"machine_id,omitempty"`
	User      *User             `json:"user,omitempty"`
	Image     *Image            `json:"image,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Network   *Network          `json:"network,omitempty"`
	Disks     *Disks            `json:"disks,omitempty"`
	// MaxSessionSeconds is the deadline of the session.
	MaxSessionSeconds int `json:"max_session_seconds,omitempty"`
}

// User is the user of the runner.
type User struct {
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Home string `json:"home"`
	// Name, when set, is a user name of the image that vm-init resolves
	// against /etc/passwd of the image; UID and GID are then the fallback.
	Name string `json:"name,omitempty"`
}

// Image is the part of the OCI image config that vm-init applies.
type Image struct {
	Env        []string `json:"env,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
}

// Network is the guest network.
type Network struct {
	// Mode is "restricted" or "unrestricted".
	Mode string `json:"mode"`
	// Address is the guest address with prefix, for example 10.213.0.14/30.
	Address string   `json:"address"`
	Gateway string   `json:"gateway"`
	MTU     int      `json:"mtu"`
	DNS     []string `json:"dns"`
	// ProxyURL and EgressReportURL are empty in unrestricted mode.
	ProxyURL        string `json:"proxy_url,omitempty"`
	NoProxy         string `json:"no_proxy,omitempty"`
	EgressReportURL string `json:"egress_report_url,omitempty"`
}

// Disks maps roles to Firecracker drive IDs.
type Disks struct {
	Image   string      `json:"image"`
	Scratch string      `json:"scratch"`
	Cache   []CacheDisk `json:"cache,omitempty"`
}

// CacheDisk is one read-only git mirror disk.
type CacheDisk struct {
	Drive string `json:"drive"`
	Key   string `json:"key"`
}

// Version is the version of the document.
const Version = 1

// Cleared returns the document that replaces the session document after
// vm-init read it.
func Cleared() Document { return Document{OpenFactory: Payload{V: Version, Cleared: true}} }

// Template returns the document of a snapshot template.
func Template() Document { return Document{OpenFactory: Payload{V: Version, Template: true}} }

// Size returns the size of the JSON document.
func (d Document) Size() int {
	data, _ := json.Marshal(d)
	return len(data)
}

// Fetch reads the document with the MMDS version 2 token flow: PUT
// /latest/api/token, then GET / with the token and Accept: application/json.
// base is http://169.254.169.254 in the guest.
func Fetch(ctx context.Context, hc *http.Client, base string) (Document, error) {
	var doc Document
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	base = strings.TrimRight(base, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/latest/api/token", nil)
	if err != nil {
		return doc, err
	}
	req.Header.Set("X-metadata-token-ttl-seconds", "60")
	resp, err := hc.Do(req)
	if err != nil {
		return doc, fmt.Errorf("mmds token: %w", err)
	}
	tok, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(bytes.TrimSpace(tok)) == 0 {
		return doc, fmt.Errorf("mmds token: %s", resp.Status)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return doc, err
	}
	req.Header.Set("X-metadata-token", string(bytes.TrimSpace(tok)))
	req.Header.Set("Accept", "application/json")
	resp, err = hc.Do(req)
	if err != nil {
		return doc, fmt.Errorf("mmds read: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return doc, fmt.Errorf("mmds read: %s", resp.Status)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("mmds read: %w", err)
	}
	if doc.OpenFactory.V != Version {
		return doc, fmt.Errorf("mmds document version %d, want %d", doc.OpenFactory.V, Version)
	}
	return doc, nil
}
