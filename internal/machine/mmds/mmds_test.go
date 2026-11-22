package mmds

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchTokenFlow(t *testing.T) {
	doc := Document{OpenFactory: Payload{V: 1, TaskID: "run_a.1", Env: map[string]string{"OF_RUN_ID": "run_a"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-metadata-token-ttl-seconds") != "60" {
				http.Error(w, "ttl", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte("tok123"))
		case r.Method == http.MethodGet && r.URL.Path == "/":
			if r.Header.Get("X-metadata-token") != "tok123" || r.Header.Get("Accept") != "application/json" {
				http.Error(w, "token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(doc)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got, err := Fetch(context.Background(), srv.Client(), srv.URL)
	if err != nil || got.OpenFactory.TaskID != "run_a.1" || got.OpenFactory.Env["OF_RUN_ID"] != "run_a" {
		t.Fatalf("fetch = %+v, %v", got, err)
	}
}

func TestFetchRejectsBadVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			_, _ = w.Write([]byte("t"))
			return
		}
		_, _ = w.Write([]byte(`{"openfactory":{"v":9}}`))
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("want a version error")
	}
}

func TestClearedAndTemplate(t *testing.T) {
	data, _ := json.Marshal(Cleared())
	if string(data) != `{"openfactory":{"v":1,"cleared":true}}` {
		t.Fatalf("cleared %s", data)
	}
	if !Template().OpenFactory.Template || Template().Size() == 0 {
		t.Fatal("template")
	}
}
