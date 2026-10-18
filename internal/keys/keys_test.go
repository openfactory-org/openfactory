package keys

import (
	"bytes"
	"errors"
	"regexp"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestVersionIsStableHex(t *testing.T) {
	v := Version(key(1))
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(v) {
		t.Fatalf("version %q is not 8 hex characters", v)
	}
	if v != Version(key(1)) || v == Version(key(2)) {
		t.Fatal("version is not a stable function of the key")
	}
}

func TestWrapUnwrapWithPreviousKey(t *testing.T) {
	ctx := t.Context()
	oldP, err := NewLocal(key(1))
	if err != nil {
		t.Fatal(err)
	}
	dek := key(9)
	wrapped, ver, err := oldP.Wrap(ctx, dek)
	if err != nil || ver != Version(key(1)) {
		t.Fatalf("wrap: %v %q", err, ver)
	}

	rotated, err := NewLocal(key(2), key(1), key(1), key(2))
	if err != nil {
		t.Fatal(err)
	}
	if rotated.CurrentVersion() != Version(key(2)) {
		t.Fatalf("current %q", rotated.CurrentVersion())
	}
	if got := rotated.Versions(); len(got) != 2 {
		t.Fatalf("duplicate keys not skipped: %v", got)
	}
	for _, v := range []string{ver, ""} {
		got, err := rotated.Unwrap(ctx, wrapped, v)
		if err != nil || !bytes.Equal(got, dek) {
			t.Fatalf("unwrap with version %q: %v", v, err)
		}
	}

	removed, _ := NewLocal(key(2))
	_, err = removed.Unwrap(ctx, wrapped, ver)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown version: %v", err)
	}
	if want := ver; err == nil || !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Fatalf("error does not name the version: %v", err)
	}
	if _, err := removed.Unwrap(ctx, wrapped, ""); err == nil {
		t.Fatal("unwrap without a matching key must fail")
	}
}

func TestNewLocalRejectsBadKey(t *testing.T) {
	if _, err := NewLocal([]byte("short")); err == nil {
		t.Fatal("short current key accepted")
	}
	if _, err := NewLocal(key(1), []byte("short")); err == nil {
		t.Fatal("short previous key accepted")
	}
}

func TestTamperedWrappedKeyFails(t *testing.T) {
	p, _ := NewLocal(key(1))
	w, v, _ := p.Wrap(t.Context(), key(5))
	w[len(w)-1] ^= 1
	if _, err := p.Unwrap(t.Context(), w, v); err == nil {
		t.Fatal("tampering not detected")
	}
}
