package runnerclient

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

var fixedTime = time.Date(2026, 11, 11, 10, 0, 0, 0, time.FixedZone("x", 3600))

func fixedClock() time.Time { return fixedTime }

func openLog(t *testing.T) (*EventLog, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := OpenEventLog(p, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, p
}

func appendN(t *testing.T, l *EventLog, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(runnerproto.EventMessage, "t1", map[string]string{"content": "hello"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppendAssignsSeq(t *testing.T) {
	l, _ := openLog(t)
	for want := int64(1); want <= 3; want++ {
		e, err := l.Append(runnerproto.EventMessage, "t1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if e.Seq != want {
			t.Errorf("seq = %d, want %d", e.Seq, want)
		}
		if !e.At.Equal(fixedTime) || e.At.Location() != time.UTC {
			t.Errorf("at = %v", e.At)
		}
	}
}

func TestReopenKeepsSeq(t *testing.T) {
	l, p := openLog(t)
	appendN(t, l, 3)
	l.Close()
	l2, err := OpenEventLog(p, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if l2.LastSeq() != 3 {
		t.Errorf("LastSeq = %d", l2.LastSeq())
	}
	e, err := l2.Append(runnerproto.EventIdle, "", nil)
	if err != nil || e.Seq != 4 {
		t.Errorf("seq %d err %v", e.Seq, err)
	}
}

func TestAfter(t *testing.T) {
	l, _ := openLog(t)
	appendN(t, l, 4)
	for _, tt := range []struct {
		after int64
		want  int
	}{{0, 4}, {2, 2}, {4, 0}} {
		evs, err := l.After(tt.after, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) != tt.want {
			t.Errorf("After(%d) = %d events, want %d", tt.after, len(evs), tt.want)
		}
		for i, e := range evs {
			if e.Seq != tt.after+int64(i)+1 {
				t.Errorf("After(%d)[%d].Seq = %d", tt.after, i, e.Seq)
			}
		}
	}
}

func TestAfterMaxBytes(t *testing.T) {
	l, _ := openLog(t)
	appendN(t, l, 10)
	all, _ := l.After(0, 0)
	small, err := l.After(0, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(small) == 0 || len(small) >= len(all) {
		t.Errorf("small = %d, all = %d", len(small), len(all))
	}
	one, err := l.After(0, 1)
	if err != nil || len(one) != 1 || one[0].Seq != 1 {
		t.Errorf("one = %v err %v", one, err)
	}
}

func TestTornLastLine(t *testing.T) {
	l, p := openLog(t)
	appendN(t, l, 2)
	l.Close()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":3,"ty`)
	f.Close()
	l2, err := OpenEventLog(p, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	if l2.LastSeq() != 2 {
		t.Fatalf("LastSeq = %d", l2.LastSeq())
	}
	e, err := l2.Append(runnerproto.EventIdle, "", nil)
	if err != nil || e.Seq != 3 {
		t.Fatalf("seq %d err %v", e.Seq, err)
	}
	l2.Close()
	l3, err := OpenEventLog(p, fixedClock)
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Close()
	evs, err := l3.After(0, 0)
	if err != nil || len(evs) != 3 {
		t.Errorf("events %d err %v", len(evs), err)
	}
}

func TestCorruptMiddleLine(t *testing.T) {
	l, p := openLog(t)
	appendN(t, l, 1)
	l.Close()
	f, _ := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString("not json\n{\"seq\":2,\"type\":\"idle\",\"at\":\"2026-11-11T00:00:00Z\"}\n")
	f.Close()
	if l3, err := OpenEventLog(p, fixedClock); err == nil {
		l3.Close()
		t.Fatal("no error for a corrupt middle line")
	}
}

func TestAppendInvalidType(t *testing.T) {
	l, _ := openLog(t)
	if _, err := l.Append("nope", "", nil); err == nil {
		t.Error("no error")
	}
	if l.LastSeq() != 0 {
		t.Errorf("LastSeq = %d", l.LastSeq())
	}
}

func TestFileMode(t *testing.T) {
	_, p := openLog(t)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
}

func TestConcurrentAppend(t *testing.T) {
	l, _ := openLog(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := l.Append(runnerproto.EventMessage, "", nil); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	evs, err := l.After(0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 200 {
		t.Fatalf("events = %d", len(evs))
	}
	seen := map[int64]int{}
	for _, e := range evs {
		seen[e.Seq]++
	}
	for s := int64(1); s <= 200; s++ {
		if seen[s] != 1 {
			t.Errorf("seq %d seen %d times", s, seen[s])
		}
	}
}
