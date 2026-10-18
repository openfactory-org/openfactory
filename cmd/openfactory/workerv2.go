package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
	"github.com/openfactory-org/openfactory/internal/server"
	"github.com/openfactory-org/openfactory/internal/worker"
)

// workerV2Flags holds the flags of the worker command that version 2 uses.
type workerV2Flags struct {
	backend, id, name, metricsAddr string
	oneShot                        bool
	capacity                       int
	labels                         map[string]string
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

// workerBackendConfig loads the configuration of the sandbox backend of a worker
// from the environment. The worker needs no master key, so it runs in dev mode.
func workerBackendConfig(backend string) (*config.Config, error) {
	lookup := func(k string) (string, bool) {
		switch k {
		case "OF_DEV":
			return "true", true
		case "OF_SANDBOX_BACKEND":
			return backend, true
		}
		return os.LookupEnv(k)
	}
	return config.LoadFrom(nil, lookup)
}

// runWorkerV2 runs the worker with protocol version 2.
func runWorkerV2(ctx context.Context, g *globals, f workerV2Flags) error {
	taskEnv, err := workerTaskEnv(os.Getenv)
	if err != nil {
		return err
	}
	cfg, err := workerBackendConfig(f.backend)
	if err != nil {
		return err
	}
	log := cfg.Logger()
	if f.backend == config.BackendLocal && !envBool("OF_DEV") {
		log.Warn("the local sandbox backend runs agents directly on this host without isolation; it is for development and tests only. Use --backend kubernetes in production")
	}
	b, image, err := server.BuildTaskBackend(cfg, log)
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
		ServerURL:    strings.TrimRight(g.server, "/"),
		APIKey:       g.apiKey,
		WorkerID:     f.id,
		Name:         f.name,
		Labels:       f.labels,
		Capacity:     f.capacity,
		Backend:      b,
		Version:      version,
		OneShot:      f.oneShot,
		TaskEnv:      taskEnv,
		DefaultImage: image,
		Metrics:      m,
		Logger:       log.With("component", "worker", "worker_id", f.id),
	})
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil // the operator stopped the worker
	}
	return err
}
