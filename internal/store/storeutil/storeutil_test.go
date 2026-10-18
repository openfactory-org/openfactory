package storeutil

import (
	"errors"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	c, err := DecodeCursor(EncodeCursor(ts, "run_abc"))
	if err != nil || c.CreatedNS != ts.UnixNano() || c.ID != "run_abc" {
		t.Fatalf("got %+v, %v", c, err)
	}
	if c, err := DecodeCursor(""); c != nil || err != nil {
		t.Fatal("empty cursor must decode to nil")
	}
	for _, bad := range []string{"!!", "bm9jb2xvbg", "eDph"} {
		if _, err := DecodeCursor(bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("DecodeCursor(%q) err = %v", bad, err)
		}
	}
	if !c.After(ts.UnixNano()-1, "zzz") || c.After(ts.UnixNano(), "run_abd") || !c.After(ts.UnixNano(), "run_abb") {
		t.Fatal("After ordering wrong")
	}
}

func TestLimitAndHost(t *testing.T) {
	for in, want := range map[int]int{-1: 50, 0: 50, 10: 10, 500: 500, 501: 500} {
		if got := Limit(in); got != want {
			t.Errorf("Limit(%d) = %d", in, got)
		}
	}
	if WorkerHost("") != "server" || WorkerHost("w1") != "w1" {
		t.Fatal("WorkerHost")
	}
}
