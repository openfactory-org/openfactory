package runnertoken

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestNewBootstrap(t *testing.T) {
	tok, hash, err := NewBootstrap()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, BootstrapPrefix) {
		t.Fatalf("bad prefix: %q", tok)
	}
	tok2, _, err := NewBootstrap()
	if err != nil {
		t.Fatal(err)
	}
	if tok == tok2 {
		t.Fatal("two tokens are equal")
	}
	h, err := HashBootstrap(tok)
	if err != nil {
		t.Fatal(err)
	}
	if h != hash {
		t.Fatalf("HashBootstrap = %q, want %q", h, hash)
	}
	if !hex64.MatchString(hash) {
		t.Fatalf("bad hash: %q", hash)
	}
}

func TestHashBootstrapMalformed(t *testing.T) {
	tests := []string{
		"", "ofb_", "ofs_xxx", "ofb_" + strings.Repeat("a", 10),
		"ofb_" + strings.Repeat("!", 43),
	}
	for _, tok := range tests {
		h, err := HashBootstrap(tok)
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: err = %v, want ErrMalformed", tok, err)
		}
		if h != "" {
			t.Errorf("%q: hash = %q, want empty", tok, h)
		}
	}
}

func TestInstanceID(t *testing.T) {
	a, err := NewInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two IDs are equal")
	}
	h := HashInstanceID(a)
	if h != HashInstanceID(a) {
		t.Fatal("hash is not stable")
	}
	if !hex64.MatchString(h) {
		t.Fatalf("bad hash: %q", h)
	}
}

func TestEqualHash(t *testing.T) {
	h := HashInstanceID("a")
	if !EqualHash(h, HashInstanceID("a")) {
		t.Error("equal hashes reported different")
	}
	if EqualHash(h, HashInstanceID("b")) {
		t.Error("different hashes reported equal")
	}
	if EqualHash(h, h[:10]) {
		t.Error("different lengths reported equal")
	}
	if !EqualHash("", "") {
		t.Error("empty strings should be equal")
	}
}
