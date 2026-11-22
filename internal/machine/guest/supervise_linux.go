//go:build linux

package guest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/openfactory-org/openfactory/internal/machine/ctlstream"
)

// supervise starts the runner, reaps children, serves the control stream
// and reports the exit.
func (g *initProc) supervise(env []string, uid, gid, maxSession int) error {
	_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: 65536, Max: 65536})
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	null, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		_ = null.Close()
		return err
	}
	g.tail = ctlstream.NewTail(4096)
	go func() {
		_, _ = io.Copy(g.tail, pr)
		_ = pr.Close()
	}()
	pid, err := syscall.ForkExec(RunnerBinary, []string{"openfactory", "runner"}, &syscall.ProcAttr{
		Dir: Workspace, Env: env, Files: []uintptr{null.Fd(), null.Fd(), pw.Fd()},
		Sys: &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, Setsid: true}, //nolint:gosec // G115: uids fit
	})
	// The runner has its own copies now.
	_ = pw.Close()
	_ = null.Close()
	if err != nil {
		return fmt.Errorf("start the runner: %w", err)
	}
	g.runnerPID = pid
	_ = g.send(ctlstream.Frame{Type: ctlstream.TypeRunnerStarted, PID: pid})

	g.runnerDone = make(chan unix.WaitStatus, 1)
	go g.reap()
	go g.readLoop()
	var deadline <-chan time.Time
	if maxSession > 0 {
		deadline = time.After(time.Duration(maxSession+60) * time.Second)
	}
	// A second cancel or the session deadline can bring the SIGKILL closer
	// but never moves it later.
	var kill killClock
	for {
		select {
		case ws := <-g.runnerDone:
			kill.stop()
			g.report(ws)
			return nil
		case grace := <-g.cancel.ch:
			_ = unix.Kill(pid, unix.SIGTERM)
			if grace <= 0 {
				_ = unix.Kill(-1, unix.SIGKILL)
			}
			kill.arm(time.Duration(grace) * time.Second)
		case <-deadline:
			_ = unix.Kill(pid, unix.SIGTERM)
			kill.arm(30 * time.Second)
		case <-kill.C():
			_ = unix.Kill(-1, unix.SIGKILL)
			kill.stop()
		}
	}
}

// reap waits for every child; the status of the runner ends supervise.
func (g *initProc) reap() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if pid == g.runnerPID {
			g.runnerDone <- ws
		}
	}
}

// readLoop serves host frames and reconnects after a broken stream (an
// agent restart): the new agent accepts the resume hello with the key.
func (g *initProc) readLoop() {
	for {
		c := g.current()
		for {
			f, err := c.Read()
			if err != nil {
				break
			}
			switch f.Type {
			case ctlstream.TypeCancel:
				g.cancel.push(f.GraceSeconds)
			case ctlstream.TypeExitAck:
				select {
				case g.exitAck <- struct{}{}:
				default:
				}
			}
		}
		_ = c.Close()
		g.mu.Lock()
		if g.conn == c {
			g.conn = nil
		}
		g.mu.Unlock()
		if err := g.connect(ctlstream.Frame{Type: ctlstream.TypeHello, V: ctlstream.Version, VMInitVersion: Version, Phase: ctlstream.PhaseResume, StreamKey: g.key, Running: true}, reconnectLimit); err != nil {
			return
		}
	}
}

// report ends every process, sends the exit frame and waits for the ack.
func (g *initProc) report(ws unix.WaitStatus) {
	_ = unix.Kill(-1, unix.SIGTERM)
	time.Sleep(2 * time.Second)
	_ = unix.Kill(-1, unix.SIGKILL)
	vmstat, _ := os.ReadFile("/proc/vmstat")
	f := ctlstream.Frame{Type: ctlstream.TypeExit, Phase: ctlstream.ExitPhaseRunner, OOMKills: OOMKills(string(vmstat)), OutputTail: g.tail.String()}
	if ws.Signaled() {
		f.Signal = int(ws.Signal())
	} else {
		code := ws.ExitStatus()
		f.ExitCode = &code
	}
	g.deliver(f)
}

// deliver sends the exit frame until the host acknowledges it, also across
// a reconnect, for at most reconnectLimit.
func (g *initProc) deliver(f ctlstream.Frame) {
	deadline := time.Now().Add(reconnectLimit)
	for time.Now().Before(deadline) {
		if g.send(f) == nil {
			select {
			case <-g.exitAck:
				return
			case <-time.After(2 * time.Second):
			}
		} else {
			time.Sleep(time.Second)
		}
	}
}

// finish reports an init error and reboots. With reboot=k the guest
// reboot ends Firecracker on both architectures.
func (g *initProc) finish(err error) {
	if err != nil {
		msg := err.Error()
		if len(msg) > 2048 {
			msg = msg[:2048]
		}
		if g.current() != nil {
			if g.exitAck == nil {
				g.exitAck = make(chan struct{}, 1)
			}
			if g.runnerPID == 0 { // no read loop yet: read the ack here
				go func() {
					c := g.current()
					if c != nil {
						if _, err := c.Expect(ctlstream.TypeExitAck, nil); err == nil {
							g.exitAck <- struct{}{}
						}
					}
				}()
			}
			_ = g.send(ctlstream.Frame{Type: ctlstream.TypeExit, Phase: ctlstream.ExitPhaseInit, Error: strings.TrimSpace(msg)})
			select {
			case <-g.exitAck:
			case <-time.After(2 * time.Second):
			}
		}
	}
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	select {}
}
