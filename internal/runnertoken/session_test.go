package runnertoken

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

var testNow = time.Unix(1794400000, 0)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func testKeys(t *testing.T, current string, keys map[string][]byte) *StaticKeys {
	t.Helper()
	k, err := NewStaticKeys(current, keys)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testClaims() Claims {
	return Claims{Team: "team_1", Run: "run_1", Session: 2, Subject: "user:usr_1"}
}

func mintTest(t *testing.T, keys KeySet) string {
	t.Helper()
	tok, _, err := Mint(keys, testClaims(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestMintVerifyRoundTrip(t *testing.T) {
	keys := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	in := testClaims()
	in.Version, in.KeyID, in.IssuedAt, in.ExpiresAt = 9, "other", 5, 6
	tok, exp, err := Mint(keys, in, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := testNow.Add(time.Hour).UTC(); !exp.Equal(want) {
		t.Fatalf("expiresAt = %v, want %v", exp, want)
	}
	got, err := Verify(keys, tok, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := testClaims()
	want.Version = 1
	want.KeyID = "k1"
	want.IssuedAt = testNow.Unix()
	want.ExpiresAt = testNow.Add(time.Hour).Unix()
	if got != want {
		t.Fatalf("claims = %+v, want %+v", got, want)
	}
	if !got.Expiry().Equal(exp) {
		t.Fatalf("Expiry = %v, want %v", got.Expiry(), exp)
	}
}

func TestTokenFormat(t *testing.T) {
	keys := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	tok := mintTest(t, keys)
	if !strings.HasPrefix(tok, "ofs_") {
		t.Fatalf("bad prefix: %q", tok)
	}
	if strings.Count(tok, ".") != 1 {
		t.Fatalf("want one dot: %q", tok)
	}
	if strings.Contains(tok, "=") {
		t.Fatalf("token has padding: %q", tok)
	}
}

func TestVerifyExpired(t *testing.T) {
	keys := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	tok := mintTest(t, keys)
	if _, err := Verify(keys, tok, testNow.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
	if _, err := Verify(keys, tok, testNow.Add(time.Hour-time.Second)); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestVerifyNotYetValid(t *testing.T) {
	keys := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	tok := mintTest(t, keys)
	if _, err := Verify(keys, tok, testNow.Add(-61*time.Second)); !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("err = %v, want ErrNotYetValid", err)
	}
	if _, err := Verify(keys, tok, testNow.Add(-60*time.Second)); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// flip returns s with the character at i changed to another base64url character.
func flip(s string, i int) string {
	b := []byte(s)
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestVerifyTampered(t *testing.T) {
	keys := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	tok := mintTest(t, keys)
	dot := strings.Index(tok, ".")

	// Change a character in the middle of P.
	bad := flip(tok, len(SessionPrefix)+(dot-len(SessionPrefix))/2)
	c, err := Verify(keys, bad, testNow)
	if !errors.Is(err, ErrBadSignature) && !errors.Is(err, ErrMalformed) {
		t.Fatalf("payload tamper: err = %v", err)
	}
	if c != (Claims{}) {
		t.Fatalf("claims not zero: %+v", c)
	}

	// Change a character at the start of S.
	bad = flip(tok, dot+1)
	c, err = Verify(keys, bad, testNow)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("signature tamper: err = %v, want ErrBadSignature", err)
	}
	if c != (Claims{}) {
		t.Fatalf("claims not zero: %+v", c)
	}
}

func TestVerifyWrongKey(t *testing.T) {
	a := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	b := testKeys(t, "k1", map[string][]byte{"k1": key(2)})
	tok := mintTest(t, a)
	c, err := Verify(b, tok, testNow)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
	if c != (Claims{}) {
		t.Fatalf("claims not zero: %+v", c)
	}
}

func TestVerifyUnknownKid(t *testing.T) {
	a := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	b := testKeys(t, "k2", map[string][]byte{"k2": key(2)})
	tok := mintTest(t, a)
	if _, err := Verify(b, tok, testNow); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestKeyRotation(t *testing.T) {
	old := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	tok := mintTest(t, old)
	both := testKeys(t, "k2", map[string][]byte{"k1": key(1), "k2": key(2)})
	if _, err := Verify(both, tok, testNow); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	only := testKeys(t, "k2", map[string][]byte{"k2": key(2)})
	if _, err := Verify(only, tok, testNow); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	keys := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	tests := []string{
		"", "ofs_", "ofs_abc", "ofs_a.b.c", "xyz_a.b", "ofs_!!!.abc",
		"ofs_" + strings.Repeat("a", 5000),
	}
	for _, tok := range tests {
		name := tok
		if len(name) > 20 {
			name = name[:20]
		}
		t.Run(name, func(t *testing.T) {
			c, err := Verify(keys, tok, testNow)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("err = %v, want ErrMalformed", err)
			}
			if c != (Claims{}) {
				t.Fatalf("claims not zero: %+v", c)
			}
		})
	}
}

func TestMintRejectsBadClaims(t *testing.T) {
	keys := testKeys(t, "k1", map[string][]byte{"k1": key(1)})
	tests := map[string]func(*Claims){
		"empty team":    func(c *Claims) { c.Team = "" },
		"empty run":     func(c *Claims) { c.Run = "" },
		"empty subject": func(c *Claims) { c.Subject = "" },
		"session zero":  func(c *Claims) { c.Session = 0 },
	}
	for name, mod := range tests {
		t.Run(name, func(t *testing.T) {
			c := testClaims()
			mod(&c)
			tok, _, err := Mint(keys, c, testNow)
			if err == nil {
				t.Fatal("want error")
			}
			if tok != "" {
				t.Fatalf("token = %q, want empty", tok)
			}
		})
	}
}

func TestNewStaticKeys(t *testing.T) {
	if _, err := NewStaticKeys("", map[string][]byte{"k1": key(1)}); err == nil {
		t.Error("empty current: want error")
	}
	if _, err := NewStaticKeys("k2", map[string][]byte{"k1": key(1)}); err == nil {
		t.Error("missing current: want error")
	}
	if _, err := NewStaticKeys("k1", map[string][]byte{"k1": make([]byte, 31)}); err == nil {
		t.Error("short key: want error")
	}

	k1 := key(1)
	m := map[string][]byte{"k1": k1}
	keys, err := NewStaticKeys("k1", m)
	if err != nil {
		t.Fatal(err)
	}
	k1[0] = 99
	m["k2"] = key(2)
	delete(m, "k1")
	got, ok := keys.Lookup("k1")
	if !ok || !bytes.Equal(got, key(1)) {
		t.Fatal("key set changed after caller edit")
	}
	if _, ok := keys.Lookup("k2"); ok {
		t.Fatal("key added after the call")
	}
	got[0] = 77
	again, _ := keys.Lookup("k1")
	if !bytes.Equal(again, key(1)) {
		t.Fatal("Lookup did not return a copy")
	}
	kid, cur := keys.Current()
	if kid != "k1" || !bytes.Equal(cur, key(1)) {
		t.Fatal("Current is wrong")
	}
}

func TestPreviousKeyVerifiesUntilRemoved(t *testing.T) {
	oldKeys := testKeys(t, "v-old", map[string][]byte{"v-old": key(1)})
	tok := mintTest(t, oldKeys)

	rotated := testKeys(t, "v-new", map[string][]byte{"v-new": key(2), "v-old": key(1)})
	if _, err := Verify(rotated, tok, testNow); err != nil {
		t.Fatalf("token of the previous key: %v", err)
	}
	if got, err := Verify(rotated, mintTest(t, rotated), testNow); err != nil || got.KeyID != "v-new" {
		t.Fatalf("new token: %+v %v", got, err)
	}
	if ids := rotated.IDs(); len(ids) != 2 {
		t.Fatalf("IDs = %v", ids)
	}

	removed := testKeys(t, "v-new", map[string][]byte{"v-new": key(2)})
	if _, err := Verify(removed, tok, testNow); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("after removal: %v, want ErrUnknownKey", err)
	}
}
