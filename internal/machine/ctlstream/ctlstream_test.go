package ctlstream

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
)

type rwc struct {
	io.Reader
	io.Writer
}

func (rwc) Close() error { return nil }

func TestRoundTripAndUnknown(t *testing.T) {
	a, b := net.Pipe()
	ca, cb := New(a), New(b)
	go func() {
		_, _ = a.Write([]byte("not json\n{\"type\":\"future\"}\n"))
		code := 3
		_ = ca.Write(Frame{Type: TypeHello, V: Version, Phase: PhaseBoot})
		_ = ca.Write(Frame{Type: TypeRunnerStarted, PID: 7})
		_ = ca.Write(Frame{Type: TypeExit, Phase: ExitPhaseRunner, ExitCode: &code, OOMKills: 1})
		_ = a.Close()
	}()
	f, err := cb.Read()
	if err != nil || f.Type != TypeHello || f.Phase != PhaseBoot || f.V != 1 {
		t.Fatalf("hello = %+v, %v", f, err)
	}
	var others []string
	f, err = cb.Expect(TypeExit, func(o Frame) { others = append(others, o.Type) })
	if err != nil || *f.ExitCode != 3 || f.OOMKills != 1 || len(others) != 1 || others[0] != TypeRunnerStarted {
		t.Fatalf("exit = %+v, %v, %v", f, err, others)
	}
	if _, err := cb.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("eof: %v", err)
	}
}

func TestSizeLimit(t *testing.T) {
	big := strings.Repeat("x", MaxFrame)
	c := New(rwc{Reader: strings.NewReader(`{"type":"exit","error":"` + big + "\"}\n"), Writer: io.Discard})
	if _, err := c.Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("read: %v", err)
	}
	if err := c.Write(Frame{Type: TypeExit, Error: big}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("write: %v", err)
	}
}

func TestTail(t *testing.T) {
	tl := NewTail(4)
	_, _ = tl.Write([]byte("abc"))
	_, _ = tl.Write([]byte("defg"))
	if tl.String() != "defg" {
		t.Fatalf("tail %q", tl.String())
	}
}
