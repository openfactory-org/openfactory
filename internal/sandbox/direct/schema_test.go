package direct

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/internal/ws"
	"github.com/openfactory-org/openfactory/internal/testkit/schematest"
)

const workerSchema = "protocol.json#"

func workerValidator(t *testing.T) *schematest.Validator {
	return schematest.New(t, "../../../schemas/worker/v2")
}

func frame(t *testing.T, typ, id, method string, params, result any, rerr *rpcError) message {
	t.Helper()
	m := message{Type: typ, ID: id, Method: method, Error: rerr}
	if params != nil {
		m.Params, _ = json.Marshal(params)
	}
	if result != nil {
		m.Result, _ = json.Marshal(result)
	}
	return m
}

func fullStatus(id string) rtcore.TaskStatus {
	code := 0
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	return rtcore.TaskStatus{TaskID: id, State: rtcore.TaskExited, Reason: rtcore.SandboxCompleted, ExitCode: &code,
		Message: "ok", SandboxID: "sbx", StartedAt: &now, EndedAt: &now}
}

// TestFramesMatchSchema marshals every frame of the Go types and validates
// it against schemas/worker/v2/protocol.json.
func TestFramesMatchSchema(t *testing.T) {
	v := workerValidator(t)
	task := testTask(1)
	full := task
	full.FactoryID = "fac"
	full.Platform.User = "1001"
	full.Resources = rtcore.TaskResources{CPU: "2", Memory: "4Gi"}
	full.NetworkPolicy = &rtcore.TaskNetworkPolicy{Mode: "restricted", AllowedHosts: []string{"a.example"}}
	full.WorkspaceSizeLimit = "10Gi"
	full.Labels = map[string]string{"k": "v"}
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	ch := AuthChallenge{Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), ConnectionID: "conn_1", Protocols: []int{1}}
	resp, err := SignChallenge(key, "w1", ch)
	if err != nil {
		t.Fatal(err)
	}
	frames := map[string]message{
		"challenge": frame(t, typeAuthChallenge, "", "", ch, nil, nil),
		"response":  frame(t, typeAuthResponse, "", "", resp, nil, nil),
		"hello-min": frame(t, typeHello, "", "", HelloV2{WorkerID: "w1", Backend: "local"}, nil, nil),
		"hello-full": frame(t, typeHello, "", "", HelloV2{
			WorkerID: "w1", Name: "W", Backend: "microvm", Labels: map[string]string{"a": "b"}, Capacity: 2, Version: "1.0.0",
			OneShot: true, Tasks: []HelloTask{{TaskID: task.TaskID, Status: fullStatus(task.TaskID)}},
			ProtocolMinor: ProtocolMinorV2, Features: []string{"git_cache"},
			Facts: &WorkerFacts{Arch: "amd64", OS: "linux", Kernel: "6.18", VCPUs: 8, MemoryMiB: 16384, DiskTotalBytes: 1, DiskFreeBytes: 1,
				FirecrackerVersion: "v1.18.0", GuestKernel: "6.18", RuntimeVersion: "1.0.0", KVM: true, Warnings: []string{"w"}},
			Slots: 4, Slot: &SlotSize{VCPUs: 2, MemoryMiB: 4096, ScratchGiB: 40}, Draining: true,
		}, nil, nil),
		"welcome":     frame(t, typeWelcome, "", "", nil, Welcome{ConnectionID: "c", HeartbeatMS: 15000}, nil),
		"heartbeat":   frame(t, typeHeartbeat, "", "", Heartbeat{Active: 1, Capacity: 2}, nil, nil),
		"assign":      frame(t, typeRequest, "1", methodTaskAssign, assignParams{Task: task}, nil, nil),
		"assign-full": frame(t, typeRequest, "1", methodTaskAssign, assignParams{Task: full}, nil, nil),
		"status":      frame(t, typeRequest, "2", methodTaskStatus, taskRef{TaskID: task.TaskID}, nil, nil),
		"destroy":     frame(t, typeRequest, "3", methodTaskDestroy, taskRef{TaskID: task.TaskID}, nil, nil),
		"cancel":      frame(t, typeRequest, "4", methodTaskCancel, cancelParams{TaskID: task.TaskID, GraceSeconds: 30}, nil, nil),
		"list":        frame(t, typeRequest, "5", methodTaskList, struct{}{}, nil, nil),
		"drain":       frame(t, typeRequest, "6", methodDrain, DrainParams{Draining: true}, nil, nil),
		"ok-status":   frame(t, typeResponse, "1", "", nil, statusBody{Status: rtcore.TaskStatus{TaskID: task.TaskID, State: rtcore.TaskAccepted}}, nil),
		"ok-list":     frame(t, typeResponse, "5", "", nil, TaskListResult{Tasks: []HelloTask{{TaskID: task.TaskID, Status: fullStatus(task.TaskID)}}}, nil),
		"ok-empty":    frame(t, typeResponse, "6", "", nil, struct{}{}, nil),
		"ok-nil":      frame(t, typeResponse, "7", "", nil, nil, nil),
		"at-capacity": frame(t, typeResponse, "1", "", nil, nil, errorForV2(rtcore.ErrAtCapacity)),
		"unknown":     frame(t, typeResponse, "1", "", nil, nil, errorForV2(errUnknownMethod)),
		"state":       frame(t, typeEvent, "", eventTaskState, statusBody{Status: fullStatus(task.TaskID)}, nil, nil),
	}
	for name, m := range frames {
		if err := v.Validate(workerSchema, m); err != nil {
			data, _ := json.Marshal(m)
			t.Errorf("%s: %v\n%s", name, err, data)
		}
	}
	for name, raw := range map[string]string{
		"ping":           `{"type":"ping"}`,
		"pong":           `{"type":"pong"}`,
		"welcome-extras": `{"type":"welcome","result":{"connection_id":"c","heartbeat_ms":15000,"min_runtime_version":"1.0.0","draining":false}}`,
	} {
		if err := v.ValidateJSON(workerSchema, []byte(raw)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, raw := range map[string]string{
		"exited without reason": `{"type":"event","method":"task.state","params":{"status":{"task_id":"run_1.1","state":"exited"}}}`,
		"bad error code":        `{"type":"response","id":"1","error":{"code":"nope","message":"x"}}`,
		"result and error":      `{"type":"response","id":"1","result":{},"error":{"code":"internal","message":"x"}}`,
		"unknown method":        `{"type":"request","id":"1","method":"task.explode","params":{}}`,
		"short signature":       `{"type":"auth.response","params":{"worker_id":"w1","alg":"ed25519","sig":"abc"}}`,
		"extra hello field":     `{"type":"hello","params":{"worker_id":"w1","backend":"x","capacity":0,"tasks":[],"color":"red"}}`,
	} {
		if err := v.ValidateJSON(workerSchema, []byte(raw)); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

// TestSchemaReasons checks that the schema lists every sandbox reason.
func TestSchemaReasons(t *testing.T) {
	v := workerValidator(t)
	for _, r := range []rtcore.SandboxReason{rtcore.SandboxCompleted, rtcore.SandboxProcessFailed, rtcore.SandboxImagePull,
		rtcore.SandboxUnschedulable, rtcore.SandboxNetworkPolicyTimeout, rtcore.SandboxInvalidSpec, rtcore.SandboxContainerOOM,
		rtcore.SandboxEvicted, rtcore.SandboxNodeLost, rtcore.SandboxDeadlineExceeded, rtcore.SandboxCancelled, rtcore.SandboxUnknown} {
		m := frame(t, typeEvent, "", eventTaskState, statusBody{Status: rtcore.TaskStatus{TaskID: "run_1.1", State: rtcore.TaskExited, Reason: r}}, nil, nil)
		if err := v.Validate(workerSchema, m); err != nil {
			t.Errorf("reason %q: %v", r, err)
		}
	}
}

func TestWorkerKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pemFile := filepath.Join(dir, "worker.pem")
	if err := os.WriteFile(pemFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	seedFile := filepath.Join(dir, "worker.seed")
	if err := os.WriteFile(seedFile, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{pemFile, seedFile} {
		k, err := LoadWorkerKey(f)
		if err != nil || !k.Equal(priv) {
			t.Fatalf("LoadWorkerKey(%s) = %v", f, err)
		}
	}
	if _, err := ParseWorkerKey([]byte("nothing")); err == nil {
		t.Error("a bad key file passed")
	}
	ch := AuthChallenge{Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), ConnectionID: "c1"}
	resp, err := SignChallenge(priv, "w1", ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChallenge(pub, "w1", ch, resp); err != nil {
		t.Fatal(err)
	}
	ch2 := ch
	ch2.ConnectionID = "c2"
	if err := VerifyChallenge(pub, "w1", ch2, resp); !errors.Is(err, rtcore.ErrUnauthorized) {
		t.Errorf("signature for another connection: %v", err)
	}
	if _, err := SignChallenge(priv, "w1", AuthChallenge{Nonce: "short", ConnectionID: "c"}); err == nil {
		t.Error("a short nonce passed")
	}
}

// TestWorkerAnswersChallenge runs the worker against a fake hub that speaks
// first, checks every frame of the worker against the schema, and exercises
// task.list and drain.
func TestWorkerAnswersChallenge(t *testing.T) {
	v := workerValidator(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	type result struct {
		err  error
		list TaskListResult
		resp []message
	}
	done := make(chan result, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := ws.Upgrade(w, r, []string{SubprotocolV2})
		if err != nil {
			done <- result{err: err}
			return
		}
		defer conn.Close()
		res := result{}
		fail := func(err error) { res.err = err; done <- res }
		send := func(m message) error {
			data, _ := json.Marshal(m)
			return conn.WriteMessage(ws.OpText, data)
		}
		read := func() (message, error) {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return message{}, err
			}
			if err := v.ValidateJSON(workerSchema, data); err != nil {
				return message{}, errors.New("frame does not match the schema: " + err.Error() + ": " + string(data))
			}
			var m message
			return m, json.Unmarshal(data, &m)
		}
		ch := AuthChallenge{Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), ConnectionID: "conn_9", Protocols: []int{1}}
		if err := send(frame(t, typeAuthChallenge, "", "", ch, nil, nil)); err != nil {
			fail(err)
			return
		}
		m, err := read()
		if err != nil {
			fail(err)
			return
		}
		var ar AuthResponse
		if m.Type != typeAuthResponse || json.Unmarshal(m.Params, &ar) != nil {
			fail(errors.New("expected auth.response, got " + m.Type))
			return
		}
		if err := VerifyChallenge(pub, "w1", ch, ar); err != nil {
			fail(err)
			return
		}
		if m, err = read(); err != nil || m.Type != typeHello {
			fail(errors.New("expected hello"))
			return
		}
		var hello HelloV2
		_ = json.Unmarshal(m.Params, &hello)
		if hello.ProtocolMinor != ProtocolMinorV2 || hello.Facts == nil || hello.Facts.VCPUs < 1 {
			fail(errors.New("hello lacks facts or protocol_minor"))
			return
		}
		_ = send(frame(t, typeWelcome, "", "", nil, Welcome{ConnectionID: "conn_9", HeartbeatMS: 60000}, nil))
		_ = send(frame(t, typeRequest, "r1", methodTaskList, struct{}{}, nil, nil))
		_ = send(frame(t, typeRequest, "r2", methodDrain, DrainParams{Draining: true}, nil, nil))
		time.Sleep(100 * time.Millisecond)
		_ = send(frame(t, typeRequest, "r3", methodTaskAssign, assignParams{Task: testTask(1)}, nil, nil))
		_ = send(frame(t, typeRequest, "r4", "task.explode", struct{}{}, nil, nil))
		for len(res.resp) < 4 {
			m, err := read()
			if err != nil {
				fail(err)
				return
			}
			if m.Type == typeResponse {
				res.resp = append(res.resp, m)
				if m.ID == "r1" {
					_ = json.Unmarshal(m.Result, &res.list)
				}
			}
		}
		done <- res
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	werr := make(chan error, 1)
	go func() {
		werr <- RunWorkerV2(ctx, WorkerV2Options{ServerURL: srv.URL, APIKey: "k", WorkerID: "w1", Backend: newFakeTasks(), Key: priv, Version: "1.0.0"})
	}()
	var res result
	select {
	case res = <-done:
	case err := <-werr:
		t.Fatalf("worker ended: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}
	if res.err != nil {
		t.Fatal(res.err)
	}
	byID := map[string]message{}
	for _, m := range res.resp {
		byID[m.ID] = m
	}
	if byID["r1"].Error != nil || res.list.Tasks == nil {
		t.Errorf("task.list: %+v", byID["r1"])
	}
	if byID["r2"].Error != nil {
		t.Errorf("drain: %+v", byID["r2"].Error)
	}
	if e := byID["r3"].Error; e == nil || e.Code != codeAtCapacity {
		t.Errorf("assign while draining: %+v", e)
	}
	if e := byID["r4"].Error; e == nil || e.Code != codeUnknownMethod {
		t.Errorf("unknown method: %+v", e)
	}
}

// TestWorkerWithoutKeyRefusesChallenge checks that a worker without a key
// stops when the hub asks for one.
func TestWorkerWithoutKeyRefusesChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := ws.Upgrade(w, r, []string{SubprotocolV2})
		if err != nil {
			return
		}
		defer conn.Close()
		ch := AuthChallenge{Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), ConnectionID: "c"}
		data, _ := json.Marshal(frame(t, typeAuthChallenge, "", "", ch, nil, nil))
		_ = conn.WriteMessage(ws.OpText, data)
		_, _, _ = conn.ReadMessage()
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := RunWorkerV2(ctx, WorkerV2Options{ServerURL: srv.URL, APIKey: "k", WorkerID: "w1", Backend: newFakeTasks()})
	if !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("err = %v, want ErrKeyRequired", err)
	}
}
