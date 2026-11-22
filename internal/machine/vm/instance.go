package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

// fcVM is one Firecracker VM of a jail and its control stream to vm-init.
type fcVM struct {
	f    *Firecracker
	spec Spec
	api  API
	pid  int
	cg   string

	mu   sync.Mutex
	ln   net.Listener
	conn *ctlstream.Conn
	// raw is the connection under conn; readDone is closed when the reader
	// of conn returned.
	raw      net.Conn
	readDone chan struct{}
	key      string
	report   *ctlstream.Frame
	killed   bool
	// closed is set by Cleanup: no listener may open after it.
	closed bool
	// killTimer ends the VM when a cancel grace ran out; killAt is its time.
	killTimer *time.Timer
	killAt    time.Time
	running   chan struct{}
	// runningOnce guards running.
	runningOnce sync.Once
	mmdsRead    chan struct{}
	exit        chan Exit
	gone        chan struct{}
	goneOnce    sync.Once
	cleanOnce   sync.Once
	cleanErr    error
}

var _ Template = (*fcVM)(nil)

func (v *fcVM) ID() string               { return v.spec.VMID }
func (v *fcVM) Running() <-chan struct{} { return v.running }
func (v *fcVM) Wait() <-chan Exit        { return v.exit }

func (v *fcVM) StreamKey() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.key
}

func (v *fcVM) closeListener() {
	v.mu.Lock()
	ln := v.ln
	v.ln = nil
	v.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// acceptBoot accepts the one connection of vm-init after a boot or a
// restore and reads its hello. No agent code runs in the guest yet. The
// wait ends at timeout or when ctx ends.
func (v *fcVM) acceptBoot(ctx context.Context, timeout time.Duration) (ctlstream.Frame, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	v.mu.Lock()
	ln := v.ln
	v.mu.Unlock()
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	var r res
	var waitErr error
	select {
	case r = <-ch:
	case <-deadline.C:
		waitErr = ErrGuestStart
	case <-ctx.Done():
		waitErr = fmt.Errorf("%w: %w", ErrGuestStart, ctx.Err())
	}
	v.closeListener()
	if waitErr != nil {
		// Closing the listener ends Accept. A connection it accepted at the
		// same moment must not leak.
		if r := <-ch; r.c != nil {
			_ = r.c.Close()
		}
		return ctlstream.Frame{}, waitErr
	}
	if r.err != nil {
		return ctlstream.Frame{}, fmt.Errorf("%w: %w", ErrGuestStart, r.err)
	}
	_ = r.c.SetReadDeadline(time.Now().Add(timeout))
	stop := context.AfterFunc(ctx, func() { _ = r.c.Close() })
	conn := ctlstream.New(r.c)
	hello, err := conn.Expect(ctlstream.TypeHello, nil)
	if !stop() || err != nil {
		_ = conn.Close()
		if err == nil {
			err = ctx.Err()
		}
		return hello, fmt.Errorf("%w: %w", ErrGuestStart, err)
	}
	_ = r.c.SetReadDeadline(time.Time{})
	v.setConn(conn, r.c)
	return hello, nil
}

// acceptResume accepts connections until one carries a resume hello with
// the stream key of this VM, then closes the listener.
func (v *fcVM) acceptResume() {
	v.mu.Lock()
	ln := v.ln
	v.mu.Unlock()
	if ln == nil {
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		conn := ctlstream.New(c)
		hello, err := conn.Expect(ctlstream.TypeHello, nil)
		key := v.StreamKey()
		if err != nil || hello.Phase != ctlstream.PhaseResume || key == "" || hello.StreamKey != key {
			v.f.log().Warn("refused a control stream without the stream key", "vm", v.spec.VMID)
			_ = conn.Close()
			continue
		}
		_ = c.SetReadDeadline(time.Time{})
		v.closeListener()
		v.setConn(conn, c)
		return
	}
}

func (v *fcVM) setConn(c *ctlstream.Conn, raw net.Conn) {
	done := make(chan struct{})
	v.mu.Lock()
	v.conn, v.raw, v.readDone = c, raw, done
	v.mu.Unlock()
	go func() {
		defer close(done)
		v.read(c)
	}()
}

func (v *fcVM) currentConn() *ctlstream.Conn {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.conn
}

// read handles the frames of vm-init on one connection.
func (v *fcVM) read(c *ctlstream.Conn) {
	for {
		fr, err := c.Read()
		if err != nil {
			v.mu.Lock()
			if v.conn == c {
				v.conn = nil
			}
			v.mu.Unlock()
			select {
			case <-v.gone:
				return
			default:
			}
			// The stream broke while the VM runs: vm-init reconnects.
			if !errors.Is(err, io.EOF) {
				v.f.log().Warn("control stream", "vm", v.spec.VMID, "err", err)
			}
			v.relisten()
			return
		}
		switch fr.Type {
		case ctlstream.TypeRunnerStarted:
			v.runningOnce.Do(func() { close(v.running) })
		case ctlstream.TypeMMDSRead:
			select {
			case v.mmdsRead <- struct{}{}:
			default:
			}
		case ctlstream.TypeExit:
			f := fr
			v.mu.Lock()
			v.report = &f
			v.mu.Unlock()
			_ = c.Write(ctlstream.Frame{Type: ctlstream.TypeExitAck})
		}
	}
}

// relisten opens the listener again for the reconnect of vm-init. It opens
// none after Cleanup.
func (v *fcVM) relisten() {
	v.mu.Lock()
	closed := v.closed
	v.mu.Unlock()
	if closed {
		return
	}
	ln, err := v.f.listen(v.spec, v.f.Jail.Root(v.spec.VMID))
	if err != nil {
		return
	}
	v.mu.Lock()
	if v.closed {
		// Cleanup ran while the listener opened.
		v.mu.Unlock()
		_ = ln.Close()
		return
	}
	v.ln = ln
	v.mu.Unlock()
	go v.acceptResume()
}

func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Go writes the session document, sends go and clears MMDS after vm-init
// read it.
func (v *fcVM) Go(ctx context.Context, doc mmds.Document) error {
	c := v.currentConn()
	if c == nil {
		return errors.New("no control stream to vm-init")
	}
	if err := v.api.PutMMDS(ctx, doc); err != nil {
		return err
	}
	key := newKey()
	v.mu.Lock()
	v.key = key
	v.mu.Unlock()
	if err := c.Write(ctlstream.Frame{Type: ctlstream.TypeGo, StreamKey: key}); err != nil {
		return err
	}
	select {
	case <-v.mmdsRead:
	case <-v.gone:
		return errors.New("the VM ended before it read its document")
	case <-time.After(30 * time.Second):
		return errors.New("vm-init did not read its document")
	case <-ctx.Done():
		return ctx.Err()
	}
	return v.api.PutMMDS(ctx, mmds.Cleared())
}

func (v *fcVM) Cancel(ctx context.Context, grace time.Duration) error {
	if grace <= 0 {
		return v.Kill(ctx)
	}
	sent := false
	if c := v.currentConn(); c != nil {
		sent = c.Write(ctlstream.Frame{Type: ctlstream.TypeCancel, GraceSeconds: int(grace / time.Second)}) == nil
	}
	if !sent && v.spec.Arch == "amd64" && v.api != nil {
		// vm-init turned Ctrl-Alt-Del into SIGINT for PID 1: a cancel with 10 s grace.
		_ = v.api.Action(ctx, ActionSendCtrlAltDel)
	}
	v.armKill(grace + 5*time.Second)
	return nil
}

// armKill kills the VM after d. A later cancel can bring the kill closer but
// never moves it later.
func (v *fcVM) armKill(d time.Duration) {
	at := time.Now().Add(d)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || (v.killTimer != nil && !at.Before(v.killAt)) {
		return
	}
	select {
	case <-v.gone:
		return
	default:
	}
	if v.killTimer != nil {
		v.killTimer.Stop()
	}
	v.killAt = at
	v.killTimer = time.AfterFunc(d, func() {
		select {
		case <-v.gone:
		default:
			_ = v.Kill(context.Background())
		}
	})
}

// stopKill stops the kill timer of a cancel.
func (v *fcVM) stopKill() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.killTimer != nil {
		v.killTimer.Stop()
	}
}

func (v *fcVM) Kill(context.Context) error {
	v.mu.Lock()
	v.killed = true
	v.mu.Unlock()
	select {
	case <-v.gone:
		return nil
	default:
	}
	if err := v.f.Cgroups.Kill(v.cg); err == nil {
		return nil
	}
	if v.pid > 1 {
		if err := syscall.Kill(v.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

// watch waits until the Firecracker process ended and sends the Exit.
func (v *fcVM) watch() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if v.pid > 1 && v.f.alive(v.pid) {
			continue
		}
		break
	}
	v.goneOnce.Do(func() { close(v.gone) })
	v.stopKill()
	v.drainReader()
	v.mu.Lock()
	e := Exit{Report: v.report, Killed: v.killed}
	v.mu.Unlock()
	e.HostOOMKills = v.f.Cgroups.OOMKills(v.cg)
	v.exit <- e
	close(v.exit)
}

// readerDrain bounds the wait of watch for the reader after the VM ended.
const readerDrain = time.Second

// drainReader waits until the reader of the control stream stored the frames
// that vm-init sent before Firecracker ended, for example an exit report.
// The read deadline ends a stream whose peer did not close.
func (v *fcVM) drainReader() {
	v.mu.Lock()
	raw, done := v.raw, v.readDone
	v.mu.Unlock()
	if done == nil {
		return
	}
	if raw != nil {
		_ = raw.SetReadDeadline(time.Now().Add(readerDrain))
	}
	t := time.NewTimer(2 * readerDrain)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

func (v *fcVM) Cleanup(ctx context.Context) error {
	v.cleanOnce.Do(func() {
		v.mu.Lock()
		v.closed = true
		v.mu.Unlock()
		v.stopKill()
		_ = v.Kill(ctx)
		if v.pid > 1 {
			deadline := time.Now().Add(5 * time.Second)
			for v.f.alive(v.pid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
		}
		v.goneOnce.Do(func() { close(v.gone) })
		if err := v.f.Cgroups.WaitEmpty(v.cg, 5*time.Second); err != nil {
			v.cleanErr = err
		}
		v.closeListener()
		if c := v.currentConn(); c != nil {
			_ = c.Close()
		}
		v.f.removeJail(v.spec.VMID)
	})
	return v.cleanErr
}

// Snapshot pauses a template VM and writes its snapshot in the jail.
func (v *fcVM) Snapshot(ctx context.Context) (SnapshotFiles, error) {
	if err := v.api.PatchVM(ctx, StatePaused); err != nil {
		return SnapshotFiles{}, err
	}
	if err := v.api.CreateSnapshot(ctx, SnapshotCreate{SnapshotType: "Full", SnapshotPath: "vm.state", MemFilePath: "mem", SyncSnapshotFiles: true}); err != nil {
		return SnapshotFiles{}, err
	}
	root := v.f.Jail.Root(v.spec.VMID)
	return SnapshotFiles{State: filepath.Join(root, "vm.state"), Mem: filepath.Join(root, "mem"), Scratch: filepath.Join(root, "scratch.ext4")}, nil
}
