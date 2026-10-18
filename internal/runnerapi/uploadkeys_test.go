package runnerapi

import (
	"bytes"
	"testing"

	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

func TestUploadIDSurvivesKeyRotation(t *testing.T) {
	mk := func(current string, keys map[string]byte) *Service {
		m := map[string][]byte{}
		for id, b := range keys {
			m[id] = bytes.Repeat([]byte{b}, 32)
		}
		ks, err := runnertoken.NewStaticKeys(current, m)
		if err != nil {
			t.Fatal(err)
		}
		return &Service{keys: ks}
	}
	const key = "runs/run_1/sessions/1/artifact/a"
	id := mk("old", map[string]byte{"old": 1}).uploadID(key)

	rotated := mk("new", map[string]byte{"new": 2, "old": 1})
	if k, ok := rotated.parseUploadID(id); !ok || k != key {
		t.Fatalf("upload ID of the previous key: %q %v", k, ok)
	}
	if k, ok := rotated.parseUploadID(rotated.uploadID(key)); !ok || k != key {
		t.Fatal("upload ID of the current key")
	}
	if _, ok := mk("new", map[string]byte{"new": 2}).parseUploadID(id); ok {
		t.Fatal("upload ID accepted after the previous key was removed")
	}
}
