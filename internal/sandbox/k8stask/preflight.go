package k8stask

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox"
	"github.com/openfactory-org/openfactory/internal/sandbox/kubernetes"
)

const (
	labelPreflight = "openfactory.dev/preflight"
	// preflightDeadline is activeDeadlineSeconds of the test Job.
	preflightDeadline = 120
	// preflightGrace is the time after which a pull error or an unschedulable
	// Pod ends the test. It is shorter than the grace time of a real task.
	preflightGrace = 30 * time.Second
)

// preflightJob creates a small Job that runs `/bin/sh -c "exit 0"` with the
// runner image, or else with the preflight image. The Job has the init
// containers of the tool images in Options.PreflightToolImages. It waits until the Job
// ends or Options.PreflightTimeout passes, deletes the Job, and returns an
// error that names the reason: image pull, unschedulable, a failed process,
// a refused Job or a timeout.
func (b *Backend) preflightJob(ctx context.Context) error {
	image := b.opts.RunnerImage
	if image == "" {
		image = b.opts.PreflightImage
	}
	if image == "" {
		image = kubernetes.DefaultImage
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := "of-preflight-" + hex.EncodeToString(rnd[:])

	task := core.Task{
		TaskID: name, RunID: name, Session: 1, Image: image,
		Env: map[string]string{}, ToolImages: b.opts.PreflightToolImages,
	}
	grace := int64(b.opts.StopGrace.Seconds())
	spec := b.podSpecFor(task, grace, "")
	for _, c := range spec["containers"].([]any) {
		if m := asMap(c); m != nil && m["name"] == taskContainer {
			m["command"] = []any{"/bin/sh", "-c", "exit 0"}
		}
	}
	labels := map[string]any{
		labelPreflight:         name,
		sandbox.LabelManagedBy: sandbox.ManagedByValue,
	}
	ann := map[string]any{annTaskID: name}
	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name": name, "namespace": b.ns,
			"labels": copyMap(labels), "annotations": copyMap(ann),
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   preflightDeadline,
			"ttlSecondsAfterFinished": 300,
			"template": map[string]any{
				"metadata": map[string]any{"labels": copyMap(labels), "annotations": copyMap(ann)},
				"spec":     spec,
			},
		},
	}
	if err := b.do(ctx, http.MethodPost, b.jobsPath(), "", job, nil); err != nil {
		if isCode(err, http.StatusUnauthorized) || isCode(err, http.StatusForbidden) {
			return fmt.Errorf("k8stask: preflight: no permission to create jobs in namespace %q: %w", b.ns, err)
		}
		return fmt.Errorf("k8stask: preflight: create test job: %w", err)
	}
	defer func() {
		// The context of the caller can be done, so the cleanup has its own.
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		body := map[string]any{"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": "Background"}
		err := b.do(cctx, http.MethodDelete, b.jobsPath()+"/"+url.PathEscape(name), "", body, nil)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			b.log.Warn("preflight: delete test job", "job", name, "err", err)
		}
	}()

	// mapStatus gives the reason. The preflight uses short timeouts.
	mo := b.opts
	mo.ImagePullTimeout = preflightGrace
	mo.UnschedulableTimeout = preflightGrace
	deadline := time.Now().Add(b.opts.PreflightTimeout)
	var last core.TaskStatus
	for {
		st, err := b.preflightStatus(ctx, name, mo)
		switch {
		case err != nil && ctx.Err() != nil:
			return fmt.Errorf("k8stask: preflight: %w", ctx.Err())
		case err != nil:
			b.log.Debug("preflight: status failed", "job", name, "err", err)
		default:
			last = st
			if st.Exited() {
				if st.Reason == core.SandboxCompleted {
					b.log.Info("preflight job completed", "job", name, "image", image)
					return nil
				}
				return preflightError(image, st)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("k8stask: preflight: timeout: the test Job did not end in %s (last state %q): "+
				"check that the cluster can pull %q and schedule the Pod",
				b.opts.PreflightTimeout, last.State, image)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("k8stask: preflight: %w", ctx.Err())
		case <-time.After(b.opts.PollInterval):
		}
	}
}

func (b *Backend) preflightStatus(ctx context.Context, name string, o Options) (core.TaskStatus, error) {
	var j jobObj
	if err := b.do(ctx, http.MethodGet, b.jobsPath()+"/"+url.PathEscape(name), "", nil, &j); err != nil {
		return core.TaskStatus{}, err
	}
	pods, err := b.listPods(ctx, labelPreflight+"="+name, name)
	if err != nil {
		return core.TaskStatus{}, err
	}
	st := mapStatus(j, pods, time.Now(), o)
	st.TaskID = name
	return st, nil
}

// preflightError names the reason that the test Job failed.
func preflightError(image string, st core.TaskStatus) error {
	var what string
	switch st.Reason {
	case core.SandboxImagePull:
		what = "image pull failed: check the image name, the registry and the image pull secrets"
	case core.SandboxUnschedulable:
		what = "unschedulable: no node can run the Pod (check resources, taints and node selectors)"
	case core.SandboxInvalidSpec:
		what = "the cluster refused the Pod (check the pod template and the admission policies)"
	case core.SandboxDeadlineExceeded:
		what = "deadline exceeded"
	default:
		what = "the test process failed (reason " + string(st.Reason) + ")"
	}
	msg := ""
	if st.Message != "" {
		msg = ": " + st.Message
	}
	return fmt.Errorf("k8stask: preflight: %s [image %q]%s", what, image, msg)
}
