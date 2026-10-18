package k8stask

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/kubernetes"
)

// fakeAPI is an httptest Kubernetes API server for Jobs and Pods. It has no
// controllers: a test adds Pods and sets statuses by hand.
type fakeAPI struct {
	t     *testing.T
	token string

	mu          sync.Mutex
	jobs        map[string]map[string]any
	pods        map[string]map[string]any
	calls       int
	deleteBody  map[string]any // last DeleteOptions for a Job
	podDeletes  []map[string]any
	patchType   string
	forbidJobs  bool
	forbidPodsL bool

	netpols       map[string]map[string]any
	forbidNetpol  bool            // create of a NetworkPolicy gives 403
	netpolDeletes []string        // names of explicit NetworkPolicy deletes
	reviewDenied  map[string]bool // verbs that the access review denies
	reviewFails   bool            // the access review gives 403
	ruleDenied    map[string]bool // "resource:verb" that the access review denies for the Role rules
	forbidJobPost bool
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{t: t, token: "test-token", jobs: map[string]map[string]any{}, pods: map[string]map[string]any{},
		netpols: map[string]map[string]any{}, reviewDenied: map[string]bool{}, ruleDenied: map[string]bool{}}
}

func dugMap(m map[string]any, path ...string) map[string]any {
	for _, k := range path {
		m, _ = m[k].(map[string]any)
	}
	return m
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "status": "Failure", "message": msg, "reason": reason, "code": code,
	})
}

// matches checks a label selector of "k=v" and "k" terms.
func matches(selector string, labels map[string]any) bool {
	if selector == "" {
		return true
	}
	for _, term := range strings.Split(selector, ",") {
		k, v, eq := strings.Cut(term, "=")
		got, ok := labels[k]
		if !ok || (eq && got != v) {
			return false
		}
	}
	return true
}

func labelsOf(o map[string]any) map[string]any {
	l, _ := o["metadata"].(map[string]any)["labels"].(map[string]any)
	return l
}

func (f *fakeAPI) list(w http.ResponseWriter, r *http.Request, store map[string]map[string]any, forbid bool, resource string) {
	if forbid {
		writeStatus(w, 403, "Forbidden", resource+" is forbidden: User cannot list resource")
		return
	}
	sel := r.URL.Query().Get("labelSelector")
	f.mu.Lock()
	defer f.mu.Unlock()
	items := []any{}
	for _, o := range store {
		if matches(sel, labelsOf(o)) {
			items = append(items, o)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apis/batch/v1/namespaces/{ns}/jobs", func(w http.ResponseWriter, r *http.Request) {
		var j map[string]any
		if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		name := j["metadata"].(map[string]any)["name"].(string)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.forbidJobPost {
			writeStatus(w, 403, "Forbidden", "jobs.batch is forbidden: User cannot create resource")
			return
		}
		if _, ok := f.jobs[name]; ok {
			writeStatus(w, 409, "AlreadyExists", `jobs.batch "`+name+`" already exists`)
			return
		}
		j["metadata"].(map[string]any)["uid"] = "uid-" + name
		f.jobs[name] = j
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(j)
	})
	mux.HandleFunc("GET /apis/batch/v1/namespaces/{ns}/jobs/{name}", func(w http.ResponseWriter, r *http.Request) {
		// Encode while holding the lock: tests change stored jobs under f.mu.
		f.mu.Lock()
		defer f.mu.Unlock()
		j, ok := f.jobs[r.PathValue("name")]
		if !ok {
			writeStatus(w, 404, "NotFound", "job not found")
			return
		}
		_ = json.NewEncoder(w).Encode(j)
	})
	mux.HandleFunc("GET /apis/batch/v1/namespaces/{ns}/jobs", func(w http.ResponseWriter, r *http.Request) {
		f.list(w, r, f.jobs, f.forbidJobs, "jobs.batch")
	})
	mux.HandleFunc("PATCH /apis/batch/v1/namespaces/{ns}/jobs/{name}", func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]any
		_ = json.NewDecoder(r.Body).Decode(&patch)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.patchType = r.Header.Get("Content-Type")
		j, ok := f.jobs[r.PathValue("name")]
		if !ok {
			writeStatus(w, 404, "NotFound", "job not found")
			return
		}
		meta := j["metadata"].(map[string]any)
		ann, _ := meta["annotations"].(map[string]any)
		if ann == nil {
			ann = map[string]any{}
			meta["annotations"] = ann
		}
		if pm, ok := patch["metadata"].(map[string]any); ok {
			if pa, ok := pm["annotations"].(map[string]any); ok {
				for k, v := range pa {
					ann[k] = v
				}
			}
		}
		_ = json.NewEncoder(w).Encode(j)
	})
	mux.HandleFunc("DELETE /apis/batch/v1/namespaces/{ns}/jobs/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := r.PathValue("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		j, ok := f.jobs[name]
		if !ok {
			writeStatus(w, 404, "NotFound", "job not found")
			return
		}
		f.deleteBody = body
		delete(f.jobs, name)
		// Background propagation: the Pods go too.
		for _, key := range []string{labelTask, labelPreflight} {
			tl := labelsOf(j)[key]
			if tl == nil {
				continue
			}
			for pn, p := range f.pods {
				if labelsOf(p)[key] == tl {
					delete(f.pods, pn)
				}
			}
		}
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("GET /api/v1/namespaces/{ns}/pods", func(w http.ResponseWriter, r *http.Request) {
		f.list(w, r, f.pods, f.forbidPodsL, "pods")
	})
	mux.HandleFunc("DELETE /api/v1/namespaces/{ns}/pods/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := r.PathValue("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.pods[name]; !ok {
			writeStatus(w, 404, "NotFound", "pod not found")
			return
		}
		body["pod"] = name
		f.podDeletes = append(f.podDeletes, body)
		delete(f.pods, name)
		_, _ = io.WriteString(w, `{}`)
	})
	const npBase = "/apis/networking.k8s.io/v1/namespaces/{ns}/networkpolicies"
	mux.HandleFunc("POST "+npBase, func(w http.ResponseWriter, r *http.Request) {
		var o map[string]any
		_ = json.NewDecoder(r.Body).Decode(&o)
		name := o["metadata"].(map[string]any)["name"].(string)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.forbidNetpol {
			writeStatus(w, 403, "Forbidden", "networkpolicies.networking.k8s.io is forbidden: User cannot create resource")
			return
		}
		if _, ok := f.netpols[name]; ok {
			writeStatus(w, 409, "AlreadyExists", "network policy exists")
			return
		}
		f.netpols[name] = o
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(o)
	})
	mux.HandleFunc("PATCH "+npBase+"/{name}", func(w http.ResponseWriter, r *http.Request) {
		var patch map[string]any
		_ = json.NewDecoder(r.Body).Decode(&patch)
		f.mu.Lock()
		defer f.mu.Unlock()
		o, ok := f.netpols[r.PathValue("name")]
		if !ok {
			writeStatus(w, 404, "NotFound", "network policy not found")
			return
		}
		if pm, ok := patch["metadata"].(map[string]any); ok {
			if refs, ok := pm["ownerReferences"]; ok {
				o["metadata"].(map[string]any)["ownerReferences"] = refs
			}
		}
		_ = json.NewEncoder(w).Encode(o)
	})
	mux.HandleFunc("DELETE "+npBase+"/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.netpols[name]; !ok {
			writeStatus(w, 404, "NotFound", "network policy not found")
			return
		}
		f.netpolDeletes = append(f.netpolDeletes, name)
		delete(f.netpols, name)
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("POST /apis/authorization.k8s.io/v1/selfsubjectaccessreviews", func(w http.ResponseWriter, r *http.Request) {
		var o map[string]any
		_ = json.NewDecoder(r.Body).Decode(&o)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.reviewFails {
			writeStatus(w, 403, "Forbidden", "cannot create selfsubjectaccessreviews")
			return
		}
		ra := dugMap(o, "spec", "resourceAttributes")
		verb, _ := ra["verb"].(string)
		if ra["resource"] != "networkpolicies" || ra["group"] != "networking.k8s.io" {
			res, _ := ra["resource"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"allowed": !f.ruleDenied[res+":"+verb]}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"allowed": !f.reviewDenied[verb]}})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			writeStatus(w, 401, "Unauthorized", "Unauthorized")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// addPod adds a Pod for the Job of a task, with the status that the caller gives.
func (f *fakeAPI) addPod(taskID, name string, created time.Time, status map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[name] = map[string]any{
		"metadata": map[string]any{
			"name":              name,
			"labels":            map[string]any{labelTask: taskLabel(taskID), "app.kubernetes.io/managed-by": "openfactory"},
			"annotations":       map[string]any{annTaskID: taskID},
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"status": status,
	}
}

// addPreflightPod adds a Pod of the test Job with the given name.
func (f *fakeAPI) addPreflightPod(job, name string, created time.Time, status map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[name] = map[string]any{
		"metadata": map[string]any{
			"name":              name,
			"labels":            map[string]any{labelPreflight: job},
			"annotations":       map[string]any{annTaskID: job},
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"status": status,
	}
}

// waitPreflightJob waits until the test Job exists and returns its name.
func (f *fakeAPI) waitPreflightJob(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for name := range f.jobs {
			if strings.HasPrefix(name, "of-preflight-") {
				f.mu.Unlock()
				return name
			}
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the test Job was not created")
	return ""
}

func (f *fakeAPI) setPodStatus(name string, status map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[name]["status"] = status
}

func (f *fakeAPI) setJobStatus(taskID string, status map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[jobName(taskID)]["status"] = status
}

func (f *fakeAPI) job(taskID string) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[jobName(taskID)]
	return j, ok
}

func (f *fakeAPI) netpol(taskID string) (map[string]any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.netpols[jobName(taskID)]
	return o, ok
}

func (f *fakeAPI) jobCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

func (f *fakeAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// startFake starts a TLS server and returns a config for it.
func startFake(t *testing.T, f *fakeAPI) *kubernetes.RestConfig {
	srv := httptest.NewTLSServer(f.handler())
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &kubernetes.RestConfig{
		Server: srv.URL, Namespace: "agents", Token: f.token,
		TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
}

func newBackend(t *testing.T, f *fakeAPI, mod func(*Options)) *Backend {
	t.Helper()
	opts := Options{Config: startFake(t, f), PollInterval: 10 * time.Millisecond}
	if mod != nil {
		mod(&opts)
	}
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testTask(run string, session int) core.Task {
	id := core.TaskID(run, session)
	return core.Task{
		TaskID: id, RunID: run, Session: session, TeamID: "team_1",
		Image:    "example.test/build:1",
		Platform: core.TaskPlatform{OS: "linux", Arch: "amd64"},
		Env: map[string]string{
			core.TaskEnvControlURL:     "https://factory.internal",
			core.TaskEnvRunID:          run,
			core.TaskEnvSession:        itoa(session),
			core.TaskEnvBootstrapToken: "ofb_secret",
			"EXTRA":                    "1",
		},
		MaxSessionSeconds: 3600,
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
