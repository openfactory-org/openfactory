package server

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
)

type fakeTaskSource struct {
	b   core.TaskBackend
	err error
}

func (f fakeTaskSource) TaskBackend(string) (core.TaskBackend, error) { return f.b, f.err }

func TestBuildTaskBackendPlugin(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{SandboxBackend: "acme"}
	if !cfg.IsPluginBackend() {
		t.Fatal("acme must be a plugin backend")
	}

	if _, _, err := buildTaskBackend(cfg, nil, log); err == nil || !strings.Contains(err.Error(), "no plugin host") {
		t.Fatalf("no host: %v", err)
	}

	v1only := errors.New("plugin acme has no SandboxBackend service; the server needs plugin protocol v2")
	_, _, err := buildTaskBackend(cfg, fakeTaskSource{err: v1only}, log)
	if !errors.Is(err, v1only) {
		t.Fatalf("v1 plugin: %v", err)
	}

	b, image, err := buildTaskBackend(cfg, fakeTaskSource{b: nil}, log)
	if err != nil || b != nil || image == "" {
		t.Fatalf("v2 plugin: %v %v %q", b, err, image)
	}
}

func TestConfigAcceptsPluginBackend(t *testing.T) {
	c := config.Defaults()
	c.Dev, c.PublicURL, c.SandboxBackend = true, "http://localhost:8080", "acme"
	if err := c.Normalize(); err != nil {
		t.Fatalf("a plugin name is rejected: %v", err)
	}
	c.SandboxBackend = "bad name!"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "OF_SANDBOX_BACKEND") {
		t.Fatalf("a bad plugin name is accepted: %v", err)
	}
}

// TestServerRunsTheRunnerPath checks that every server has the runner path:
// the dispatcher, the runner API and the planner. There is no other path.
func TestServerRunsTheRunnerPath(t *testing.T) {
	srv, err := New(t.Context(), testConfig(t), WithLogger(quietLogger()), WithDefinitionSyncInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.runner == nil || srv.runner.dispatcher == nil || srv.runner.service == nil {
		t.Fatalf("runner mode = %+v", srv.runner)
	}
	if _, err := srv.runner.content.planner(); err != nil {
		t.Fatalf("the planner is not set: %v", err)
	}
}
