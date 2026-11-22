package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
	"github.com/openfactory-org/openfactory/internal/sandbox/tasklocal"
	"github.com/openfactory-org/openfactory/internal/worker"
)

// workerV2Flags holds the flags of the worker command that version 2 uses.
type workerV2Flags struct {
	backend, id, name, metricsAddr, keyFile string
	oneShot                                 bool
	capacity                                int
	labels                                  map[string]string
}

// workerTaskEnv returns the variables that the worker adds to each task.
// OF_IDLE_TIMEOUT_MAX_SECONDS of the worker environment is one of them.
func workerTaskEnv(lookup func(string) string) (map[string]string, error) {
	env := map[string]string{}
	if v := strings.TrimSpace(lookup("OF_IDLE_TIMEOUT_MAX_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			return nil, fmt.Errorf("OF_IDLE_TIMEOUT_MAX_SECONDS %q must be a whole number of seconds, 0 or more", v)
		}
		env["OF_IDLE_TIMEOUT_MAX_SECONDS"] = v
	}
	return env, nil
}

// localBackendOptions reads the options of the process backend from the
// environment: OF_SANDBOX_ROOT, OF_DATA_DIR, OF_RUNNER_COMMAND and
// OF_RUNNER_EXEC_COMMAND.
func localBackendOptions(lookup func(string) string, executable func() (string, error)) (tasklocal.Options, error) {
	command := strings.Fields(lookup("OF_RUNNER_COMMAND"))
	if len(command) == 0 {
		exe, err := executable()
		if err != nil {
			return tasklocal.Options{}, fmt.Errorf("find the runner command: %w", err)
		}
		command = []string{exe, "runner"}
	}
	root := strings.TrimSpace(lookup("OF_SANDBOX_ROOT"))
	if root == "" {
		data := strings.TrimSpace(lookup("OF_DATA_DIR"))
		if data == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return tasklocal.Options{}, fmt.Errorf("set OF_SANDBOX_ROOT: %w", err)
			}
			data = filepath.Join(home, ".openfactory")
		}
		root = filepath.Join(data, "sandboxes")
	}
	extra := map[string]string{}
	if c := lookup("OF_RUNNER_EXEC_COMMAND"); c != "" {
		extra["OF_RUNNER_EXEC_COMMAND"] = c
	}
	return tasklocal.Options{Root: filepath.Join(root, "tasks"), Command: command, ExtraEnv: extra}, nil
}

// buildWorkerBackend builds the process backend of the dev worker.
func buildWorkerBackend(lookup func(string) string) (rtcore.TaskBackend, error) {
	opts, err := localBackendOptions(lookup, os.Executable)
	if err != nil {
		return nil, err
	}
	return tasklocal.New(opts)
}

// runWorkerV2 runs the worker with protocol version 2.
func runWorkerV2(ctx context.Context, g *globals, f workerV2Flags) error {
	taskEnv, err := workerTaskEnv(os.Getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Warn("the process worker runs agents directly on this host without isolation; it is for development and tests only")
	b, err := buildWorkerBackend(os.Getenv)
	if err != nil {
		return err
	}
	m := worker.NewMetrics(f.id, f.backend, version, f.capacity)
	if f.metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", m.Handler())
		hs := &http.Server{Addr: f.metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("worker metrics listener", "err", err)
			}
		}()
		defer hs.Close()
		log.Info("worker metrics listening", "addr", f.metricsAddr)
	}
	err = direct.RunWorkerV2(ctx, direct.WorkerV2Options{
		ServerURL: strings.TrimRight(g.server, "/"),
		APIKey:    g.apiKey,
		WorkerID:  f.id,
		Name:      f.name,
		Labels:    f.labels,
		Capacity:  f.capacity,
		Backend:   b,
		Version:   version,
		OneShot:   f.oneShot,
		TaskEnv:   taskEnv,
		KeyFile:   f.keyFile,
		Metrics:   m,
		Logger:    log.With("component", "worker", "worker_id", f.id),
	})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil // the operator stopped the worker
	}
	return err
}
