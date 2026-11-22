package backend

import "syscall"

// killProcess stops a helper process of an earlier agent.
func killProcess(pid int) {
	if pid > 1 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
