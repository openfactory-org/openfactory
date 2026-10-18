package auth

import (
	"bytes"
	"testing"
	"time"
)

func TestSealerOpensWithPreviousKey(t *testing.T) {
	oldKey, newKey := bytes.Repeat([]byte{1}, SessionKeySize), bytes.Repeat([]byte{2}, SessionKeySize)
	now := time.Now()
	oldS, err := newSealer(oldKey, "session")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := oldS.seal(map[string]string{"u": "usr_1"}, "cookie", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := newSealer(newKey, "session", oldKey)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := rotated.open(tok, "cookie", now, &got); err != nil || got["u"] != "usr_1" {
		t.Fatalf("open with previous key: %v %v", got, err)
	}
	// New cookies use the new key only.
	fresh, _ := rotated.seal(map[string]string{"u": "usr_2"}, "cookie", now.Add(time.Hour))
	onlyNew, _ := newSealer(newKey, "session")
	if err := onlyNew.open(fresh, "cookie", now, &got); err != nil {
		t.Fatalf("new cookie with new key: %v", err)
	}
	// Once the previous key is gone, the old cookie fails.
	if err := onlyNew.open(tok, "cookie", now, &got); err == nil {
		t.Fatal("old cookie opened without the previous key")
	}
	if _, err := newSealer(newKey, "session", []byte("short")); err == nil {
		t.Fatal("short previous key accepted")
	}
}
