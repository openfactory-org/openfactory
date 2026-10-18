package sqlstore

import "testing"

func TestStripJSONNul(t *testing.T) {
	cases := map[string]string{
		`"plain"`:              `"plain"`,
		`"a\u0000b"`:           `"a\ufffdb"`,
		`"lit \\u0000 here"`:   `"lit \\u0000 here"`,
		`"\\\u0000"`:           `"\\\ufffd"`,
		`{"k":"\u0000\u0000"}`: `{"k":"\ufffd\ufffd"}`,
	}
	for in, want := range cases {
		if got := StripJSONNul(in); got != want {
			t.Errorf("StripJSONNul(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestRebind(t *testing.T) {
	s := &Store{d: Dialect{NumberedPlaceholders: true}}
	if got := s.rebind("a=? AND b IN (?,?)"); got != "a=$1 AND b IN ($2,$3)" {
		t.Fatalf("rebind = %s", got)
	}
	s.d.NumberedPlaceholders = false
	if got := s.rebind("a=?"); got != "a=?" {
		t.Fatalf("rebind = %s", got)
	}
}
