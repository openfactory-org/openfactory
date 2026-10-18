package k8stask

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
	"github.com/openfactory-org/openfactory/internal/sandbox/kubernetes"
)

const (
	taskContainer = kubernetes.TaskContainer
	installName   = "runner-install"

	workspaceDir = "/workspace"
	stateDir     = "/var/lib/openfactory/state"
	homeDir      = "/home/agent"
	runnerDir    = "/opt/openfactory"
	runnerBin    = runnerDir + "/openfactory"
	runnerSource = "/usr/local/bin/openfactory"

	toolsDir   = runnerDir + "/tools"
	toolSource = "/opt/openfactory/tool"
	toolDest   = "/dest"
	// toolCopyCommand copies the content of a tool image into the volume. It
	// does not use cp -a: that option also sets the owner, the mode and the
	// times of the target directory, and a user that is not root cannot do
	// that for the root-owned volume. The option -P keeps symbolic links.
	toolCopyCommand = "cp -RP " + toolSource + "/. " + toolDest + "/"
	toolPrefix      = "tool-"
	maxToolName     = 40

	// initUser is the user of the init containers when the Pod uses the user of the image.
	initUser = 1000

	jobPrefix = "of-task-"
	hashLen   = 8
)

var (
	toolNameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	dnsSanitizer   = regexp.MustCompile(`[^a-z0-9-]+`)
	labelSanitizer = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
)

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:hashLen]
}

// jobName returns a DNS-safe name for a task ID: the prefix "of-task-", a
// sanitized and cut form of the ID, a dash and a hash of the exact ID. The
// hash is always there. Different IDs can have the same sanitized form (for
// example "a_1" and "a.1"), and the hash keeps their names apart.
func jobName(taskID string) string {
	body := strings.Trim(dnsSanitizer.ReplaceAllString(strings.ToLower(taskID), "-"), "-")
	max := 63 - len(jobPrefix) - 1 - hashLen
	if len(body) > max {
		body = strings.TrimRight(body[:max], "-")
	}
	if body == "" {
		return jobPrefix + shortHash(taskID)
	}
	return jobPrefix + body + "-" + shortHash(taskID)
}

// taskLabel returns a label value for a task ID. A valid ID of up to 63
// characters is used as it is. Else the value is a cut form with a hash.
func taskLabel(taskID string) string {
	v := labelSanitizer.ReplaceAllString(taskID, "-")
	if v == taskID && len(v) <= 63 && v == strings.Trim(v, "-_.") {
		return v
	}
	if len(v) > 63-1-hashLen {
		v = v[:63-1-hashLen]
	}
	v = strings.Trim(v, "-_.")
	if v == "" {
		return shortHash(taskID)
	}
	return v + "-" + shortHash(taskID)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// setDefault sets m[key] only if the template did not set it.
func setDefault(m map[string]any, key string, v any) {
	if _, ok := m[key]; !ok {
		m[key] = v
	}
}

// jobManifest builds the Job of a task.
func (b *Backend) jobManifest(task core.Task) map[string]any {
	name := jobName(task.TaskID)

	labels := map[string]any{}
	for _, src := range []map[string]string{b.opts.ExtraLabels, task.Labels} {
		for k, v := range src {
			labels[k] = v
		}
	}
	labels[labelTask] = taskLabel(task.TaskID)
	labels[sandbox.LabelRun] = kubernetes.LabelValue(task.RunID)
	labels[sandbox.LabelManagedBy] = sandbox.ManagedByValue
	annotations := map[string]any{}
	for k, v := range b.opts.ExtraAnnotations {
		annotations[k] = v
	}
	annotations[annTaskID] = task.TaskID

	// A finished Job is deleted after the same time, for both outcomes. The
	// control plane calls Destroy earlier for a task that completed.
	ttl := task.FailureRetentionSeconds
	if ttl <= 0 {
		ttl = int(b.opts.FailureRetention.Seconds())
	}
	grace := int64(b.opts.StopGrace.Seconds())
	deadline := int64(task.MaxSessionSeconds) + grace
	if d := int64(b.opts.ActiveDeadline.Seconds()); d > 0 && d < deadline {
		deadline = d
	}

	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   b.ns,
			"labels":      copyMap(labels),
			"annotations": copyMap(annotations),
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   deadline,
			"ttlSecondsAfterFinished": ttl,
			"template": map[string]any{
				"metadata": map[string]any{
					"labels":      copyMap(labels),
					"annotations": copyMap(annotations),
				},
				"spec": b.podSpec(task, grace),
			},
		},
	}
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// podSpec merges the template of the operator with the fields that the task needs.
func (b *Backend) podSpec(task core.Task, grace int64) map[string]any {
	runnerImage := task.RunnerImage
	if runnerImage == "" {
		runnerImage = b.opts.RunnerImage
	}
	return b.podSpecFor(task, grace, runnerImage)
}

// podSpecFor builds the Pod spec with a given runner image. An empty image
// means that the task image holds the runner.
func (b *Backend) podSpecFor(task core.Task, grace int64, runnerImage string) map[string]any {
	spec := kubernetes.DeepCopy(b.template).(map[string]any)
	spec["restartPolicy"] = "Never"
	spec["terminationGracePeriodSeconds"] = grace

	// With RunAsImageUser, the Pod uses the user of the image and the backend
	// sets no user, group or fsGroup.
	// The user comes from platform.linux.user of the machine (task.Platform.User),
	// else from the setting of the backend.
	if user, image := b.podUser(task); !image {
		sc := asMap(spec["securityContext"])
		if sc == nil {
			sc = map[string]any{}
		}
		setDefault(sc, "runAsNonRoot", true)
		setDefault(sc, "runAsUser", user)
		setDefault(sc, "runAsGroup", user)
		setDefault(sc, "fsGroup", user)
		spec["securityContext"] = sc
	}

	// The container "sandbox".
	containers, _ := spec["containers"].([]any)
	var tc map[string]any
	for _, c := range containers {
		if m := asMap(c); m != nil && m["name"] == taskContainer {
			tc = m
			break
		}
	}
	if tc == nil {
		tc = map[string]any{"name": taskContainer}
		containers = append(containers, tc)
	}
	spec["containers"] = containers
	tc["image"] = task.Image
	setDefault(tc, "imagePullPolicy", b.opts.ImagePullPolicy)
	tc["workingDir"] = workspaceDir
	delete(tc, "args")
	tc["command"] = taskCommand(task, runnerImage != "")
	env := task.Env
	if pe := b.proxyEnv(task); pe != nil {
		env = make(map[string]string, len(task.Env)+len(pe))
		for k, v := range task.Env {
			env[k] = v
		}
		for k, v := range pe {
			env[k] = v
		}
	}
	tc["env"] = mergeEnv(tc["env"], env)
	applyResources(tc, task.Resources)

	mounts := []mount{
		{"workspace", workspaceDir},
		{"state", "/var/lib/openfactory"},
		{"home", homeDir},
	}
	sizeLimit := task.WorkspaceSizeLimit
	if sizeLimit == "" {
		sizeLimit = b.opts.WorkspaceSizeLimit
	}
	vols := []volume{
		{"workspace", sizeLimit},
		{"state", ""},
		{"home", ""},
	}
	if runnerImage != "" {
		mounts = append(mounts, mount{"runner", runnerDir})
		vols = append(vols, volume{"runner", ""})
	}
	for _, ti := range task.ToolImages {
		mounts = append(mounts, mount{toolPrefix + ti.Name, toolsDir + "/" + ti.Name})
		vols = append(vols, volume{toolPrefix + ti.Name, ""})
	}
	tc["volumeMounts"] = mergeMounts(tc["volumeMounts"], mounts)
	spec["volumes"] = mergeVolumes(spec["volumes"], vols)

	// Init containers copy the runner and the tool images into the volumes.
	// Each one replaces a container of the template that has the same name.
	var added []map[string]any
	if runnerImage != "" {
		added = append(added, b.copyContainer(installName, runnerImage,
			[]any{"cp", runnerSource, runnerBin}, "runner", runnerDir))
	}
	for _, ti := range task.ToolImages {
		added = append(added, b.copyContainer(toolPrefix+ti.Name, ti.Image,
			[]any{"/bin/sh", "-c", toolCopyCommand},
			toolPrefix+ti.Name, toolDest))
	}
	if len(added) > 0 {
		own := map[string]bool{}
		for _, a := range added {
			own[a["name"].(string)] = true
		}
		init, _ := spec["initContainers"].([]any)
		var kept []any
		for _, c := range init {
			if m := asMap(c); m == nil || !own[m["name"].(string)] {
				kept = append(kept, c)
			}
		}
		for _, a := range added {
			kept = append(kept, a)
		}
		spec["initContainers"] = kept
	}

	if b.waitActive(task) {
		// The wait runs before every container that holds task code: the
		// template containers, the copy of the runner and the copy of the
		// tool images. The sidecar and the lock go in front of it.
		init, _ := spec["initContainers"].([]any)
		spec["initContainers"] = egress.WithWait(init, b.egressWait(task, runnerImage))
	}
	if b.sidecarActive(task) {
		init, _ := spec["initContainers"].([]any)
		spec["initContainers"] = withProxySidecar(init, b.proxySidecar(task))
	}
	if b.lockActive(task) {
		// The lock runs first. The task container must not be able to become
		// the user of the proxy.
		init, _ := spec["initContainers"].([]any)
		spec["initContainers"] = egress.WithLock(init, b.egressLock(task))
		egress.HardenTask(tc)
	}

	if len(b.opts.ImagePullSecrets) > 0 {
		secrets, _ := spec["imagePullSecrets"].([]any)
		have := map[string]bool{}
		for _, s := range secrets {
			if n, _ := asMap(s)["name"].(string); n != "" {
				have[n] = true
			}
		}
		for _, n := range b.opts.ImagePullSecrets {
			if !have[n] {
				secrets = append(secrets, map[string]any{"name": n})
				have[n] = true
			}
		}
		spec["imagePullSecrets"] = secrets
	}
	return spec
}

// copyContainer returns an init container that runs a copy command with the
// volume at mountPath. It runs as a user that is not root.
func (b *Backend) copyContainer(name, image string, command []any, volumeName, mountPath string) map[string]any {
	user := b.opts.RunAsUser
	if b.opts.RunAsImageUser || user <= 0 {
		user = initUser
	}
	return map[string]any{
		"name":            name,
		"image":           image,
		"imagePullPolicy": b.opts.ImagePullPolicy,
		"command":         command,
		"securityContext": map[string]any{
			"runAsNonRoot":             true,
			"runAsUser":                user,
			"allowPrivilegeEscalation": false,
		},
		"volumeMounts": []any{map[string]any{"name": volumeName, "mountPath": mountPath}},
	}
}

// taskCommand returns the command of the container "sandbox". It is the runner.
// With tool images, a shell puts the bin directory of each tool in front of
// the PATH of the image, and then starts the runner. The runner and the
// harness inherit that PATH. A fixed PATH variable would replace the PATH of
// the image, and the backend does not know it.
func taskCommand(task core.Task, runnerImage bool) []any {
	runner := []any{"openfactory", "runner"}
	if runnerImage {
		runner = []any{runnerBin, "runner"}
	}
	if len(task.ToolImages) == 0 {
		return runner
	}
	return append([]any{"/bin/sh", "-c", toolPathScript(task.ToolImages), "openfactory-task"}, runner...)
}

// toolPathScript returns the shell script that sets the PATH and starts the
// command that follows it. The tool names are checked by validateToolImages.
func toolPathScript(images []core.TaskImage) string {
	dirs := make([]string, 0, len(images))
	for _, ti := range images {
		dirs = append(dirs, toolsDir+"/"+ti.Name+"/bin")
	}
	return `PATH="` + strings.Join(dirs, ":") + `${PATH:+:$PATH}"; export PATH; exec "$@"`
}

// validateToolImages checks the names and the images of the tool images.
func validateToolImages(images []core.TaskImage) error {
	seen := map[string]bool{}
	for _, ti := range images {
		switch {
		case len(ti.Name) > maxToolName || !toolNameRe.MatchString(ti.Name):
			return fmt.Errorf("tool image name %q must be lower case letters, digits and dashes (at most %d characters)", ti.Name, maxToolName)
		case ti.Image == "":
			return fmt.Errorf("tool image %q has no image", ti.Name)
		case seen[ti.Name]:
			return fmt.Errorf("tool image %q is listed two times", ti.Name)
		}
		seen[ti.Name] = true
	}
	return nil
}

// mergeEnv returns the variables of the template and the task, sorted by name.
// The fixed paths win over the task, and the task wins over the template.
func mergeEnv(template any, task map[string]string) []any {
	vars := map[string]any{}
	items, _ := template.([]any)
	for _, e := range items {
		if n, _ := asMap(e)["name"].(string); n != "" {
			vars[n] = e
		}
	}
	for k, v := range task {
		vars[k] = map[string]any{"name": k, "value": v}
	}
	for k, v := range map[string]string{
		"OF_WORKSPACE":        workspaceDir,
		"OF_RUNNER_STATE_DIR": stateDir,
		"HOME":                homeDir,
	} {
		vars[k] = map[string]any{"name": k, "value": v}
	}
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, vars[n])
	}
	return out
}

// applyResources sets requests equal to limits for each resource of the task.
func applyResources(tc map[string]any, r core.TaskResources) {
	if r.CPU == "" && r.Memory == "" {
		return
	}
	res := asMap(tc["resources"])
	if res == nil {
		res = map[string]any{}
	}
	for _, kind := range []string{"requests", "limits"} {
		m := asMap(res[kind])
		if m == nil {
			m = map[string]any{}
		}
		if r.CPU != "" {
			m["cpu"] = r.CPU
		}
		if r.Memory != "" {
			m["memory"] = r.Memory
		}
		res[kind] = m
	}
	tc["resources"] = res
}

type mount struct{ name, path string }

type volume struct{ name, sizeLimit string }

// mergeMounts keeps the template mounts that have another name.
func mergeMounts(template any, ours []mount) []any {
	names := map[string]bool{}
	for _, m := range ours {
		names[m.name] = true
	}
	var out []any
	items, _ := template.([]any)
	for _, e := range items {
		if n, _ := asMap(e)["name"].(string); !names[n] {
			out = append(out, e)
		}
	}
	for _, m := range ours {
		out = append(out, map[string]any{"name": m.name, "mountPath": m.path})
	}
	return out
}

func mergeVolumes(template any, ours []volume) []any {
	names := map[string]bool{}
	for _, v := range ours {
		names[v.name] = true
	}
	var out []any
	items, _ := template.([]any)
	for _, e := range items {
		if n, _ := asMap(e)["name"].(string); !names[n] {
			out = append(out, e)
		}
	}
	for _, v := range ours {
		ed := map[string]any{}
		if v.sizeLimit != "" {
			ed["sizeLimit"] = v.sizeLimit
		}
		out = append(out, map[string]any{"name": v.name, "emptyDir": ed})
	}
	return out
}

// podUser returns the numeric user of the Pod of a task. With image set, the
// Pod keeps the user of the image. The runner can set the user with
// platform.linux.user: a number or "image". A value that is not valid is
// ignored, because the definition compiler already refused it.
func (b *Backend) podUser(task core.Task) (user int64, image bool) {
	switch u := strings.TrimSpace(task.Platform.User); {
	case u == "":
	case strings.EqualFold(u, "image"):
		return 0, true
	default:
		if n, err := strconv.ParseInt(u, 10, 64); err == nil && n >= 1 {
			return n, false
		}
	}
	return b.opts.RunAsUser, b.opts.RunAsImageUser
}
