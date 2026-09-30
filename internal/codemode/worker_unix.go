//go:build unix

package codemode

import (
	"os/exec"
	"syscall"
	"time"
)

// applyMemoryCap bounds the process's data segment (heap and anonymous
// mappings) so one allocation past mib MiB fails instead of growing the
// process. RLIMIT_DATA is the limit that works with the Go runtime: an
// address-space limit of the same size refuses the runtime's own
// reservations, an RSS limit is not enforced on Linux.
func applyMemoryCap(mib int) error {
	n := uint64(mib) << 20
	return syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: n, Max: n})
}

// applyCPUCap is the backstop behind the step limit and the parent's wall
// clock: a worker still computing well past its deadline is killed by the
// kernel.
func applyCPUCap(timeout time.Duration) {
	secs := uint64(timeout/time.Second) + 10
	_ = syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: secs, Max: secs})
}

// ownProcessGroup starts the worker in its own process group so the whole
// group can be killed at once.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup ends the worker and anything it started.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
