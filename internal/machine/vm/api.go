// Package vm runs Firecracker microVMs under the jailer. The VMM interface
// hides Firecracker so that the backend tests use fakes. The Firecracker API
// client speaks to the unix-socket API with net/http and no SDK.
package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// TokenBucket is a Firecracker rate limiter bucket.
type TokenBucket struct {
	Size       int64 `json:"size"`
	RefillTime int64 `json:"refill_time"`
}

// RateLimiter limits bandwidth (bytes) and operations.
type RateLimiter struct {
	Bandwidth *TokenBucket `json:"bandwidth,omitempty"`
	Ops       *TokenBucket `json:"ops,omitempty"`
}

// MachineConfig is PUT /machine-config.
type MachineConfig struct {
	VCPUCount       int    `json:"vcpu_count"`
	MemSizeMiB      int    `json:"mem_size_mib"`
	SMT             bool   `json:"smt"`
	TrackDirtyPages bool   `json:"track_dirty_pages"`
	HugePages       string `json:"huge_pages"`
}

// BootSource is PUT /boot-source.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

// Drive is PUT /drives/{id}.
type Drive struct {
	DriveID      string       `json:"drive_id"`
	PathOnHost   string       `json:"path_on_host"`
	IsRootDevice bool         `json:"is_root_device"`
	IsReadOnly   bool         `json:"is_read_only"`
	CacheType    string       `json:"cache_type,omitempty"`
	Discard      bool         `json:"discard,omitempty"`
	IOEngine     string       `json:"io_engine,omitempty"`
	RateLimiter  *RateLimiter `json:"rate_limiter,omitempty"`
}

// NetworkInterface is PUT /network-interfaces/{id}.
type NetworkInterface struct {
	IfaceID       string       `json:"iface_id"`
	HostDevName   string       `json:"host_dev_name"`
	GuestMAC      string       `json:"guest_mac"`
	MTU           int          `json:"mtu,omitempty"`
	TxRateLimiter *RateLimiter `json:"tx_rate_limiter,omitempty"`
	RxRateLimiter *RateLimiter `json:"rx_rate_limiter,omitempty"`
}

// MMDSConfig is PUT /mmds/config.
type MMDSConfig struct {
	Version           string   `json:"version"`
	NetworkInterfaces []string `json:"network_interfaces"`
	IPv4Address       string   `json:"ipv4_address"`
	IMDSCompat        bool     `json:"imds_compat"`
}

// Vsock is PUT /vsock.
type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// Entropy is PUT /entropy.
type Entropy struct {
	RateLimiter *RateLimiter `json:"rate_limiter,omitempty"`
}

// SnapshotCreate is PUT /snapshot/create.
type SnapshotCreate struct {
	SnapshotType      string `json:"snapshot_type"`
	SnapshotPath      string `json:"snapshot_path"`
	MemFilePath       string `json:"mem_file_path"`
	SyncSnapshotFiles bool   `json:"sync_snapshot_files"`
}

// MemBackend is the memory backend of a snapshot load.
type MemBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

// NetworkOverride points a restored interface at another tap.
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// SnapshotLoad is PUT /snapshot/load.
type SnapshotLoad struct {
	SnapshotPath     string            `json:"snapshot_path"`
	MemBackend       MemBackend        `json:"mem_backend"`
	TrackDirtyPages  bool              `json:"track_dirty_pages"`
	ResumeVM         bool              `json:"resume_vm"`
	NetworkOverrides []NetworkOverride `json:"network_overrides,omitempty"`
	ClockRealtime    bool              `json:"clock_realtime,omitempty"`
}

// Actions of PUT /actions.
const (
	ActionInstanceStart  = "InstanceStart"
	ActionSendCtrlAltDel = "SendCtrlAltDel"
)

// VM states of PATCH /vm.
const (
	StatePaused  = "Paused"
	StateResumed = "Resumed"
)

// API is the Firecracker API that the VMM uses.
type API interface {
	PutMachineConfig(ctx context.Context, c MachineConfig) error
	PutBootSource(ctx context.Context, b BootSource) error
	PutDrive(ctx context.Context, d Drive) error
	PutNetworkInterface(ctx context.Context, n NetworkInterface) error
	PutMMDSConfig(ctx context.Context, c MMDSConfig) error
	PutMMDS(ctx context.Context, doc any) error
	PutVsock(ctx context.Context, v Vsock) error
	PutEntropy(ctx context.Context, e Entropy) error
	Action(ctx context.Context, action string) error
	PatchVM(ctx context.Context, state string) error
	CreateSnapshot(ctx context.Context, p SnapshotCreate) error
	LoadSnapshot(ctx context.Context, p SnapshotLoad) error
}

// Client is the Firecracker API over its unix socket.
type Client struct {
	hc *http.Client
}

// NewClient returns a client of the API socket at path.
func NewClient(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}
	return &Client{hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// Error is a non-2xx answer of the API.
type Error struct {
	Method, Path string
	Status       int
	Fault        string
}

func (e *Error) Error() string {
	return fmt.Sprintf("firecracker %s %s: %d: %s", e.Method, e.Path, e.Status, e.Fault)
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://fc"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var f struct {
		FaultMessage string `json:"fault_message"`
	}
	if json.Unmarshal(out, &f) != nil || f.FaultMessage == "" {
		f.FaultMessage = strings.TrimSpace(string(out))
	}
	return &Error{Method: method, Path: path, Status: resp.StatusCode, Fault: f.FaultMessage}
}

func (c *Client) PutMachineConfig(ctx context.Context, m MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", m)
}
func (c *Client) PutBootSource(ctx context.Context, b BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", b)
}
func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d)
}
func (c *Client) PutNetworkInterface(ctx context.Context, n NetworkInterface) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+n.IfaceID, n)
}
func (c *Client) PutMMDSConfig(ctx context.Context, m MMDSConfig) error {
	return c.do(ctx, http.MethodPut, "/mmds/config", m)
}
func (c *Client) PutMMDS(ctx context.Context, doc any) error {
	return c.do(ctx, http.MethodPut, "/mmds", doc)
}
func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.do(ctx, http.MethodPut, "/vsock", v)
}
func (c *Client) PutEntropy(ctx context.Context, e Entropy) error {
	return c.do(ctx, http.MethodPut, "/entropy", e)
}
func (c *Client) Action(ctx context.Context, action string) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": action})
}
func (c *Client) PatchVM(ctx context.Context, state string) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": state})
}
func (c *Client) CreateSnapshot(ctx context.Context, p SnapshotCreate) error {
	return c.do(ctx, http.MethodPut, "/snapshot/create", p)
}
func (c *Client) LoadSnapshot(ctx context.Context, p SnapshotLoad) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", p)
}
