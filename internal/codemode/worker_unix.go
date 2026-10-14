//go:build unix

package codemode

import (
	"os"
	"os/exec"
	"os/signal"
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

// ignoreStopSignals keeps a running script alive when a service manager
// signals every process of the unit (systemd's default KillMode sends
// SIGTERM to the whole control group). The gateway alone decides when a
// worker ends: it drains on SIGTERM and stops a worker with SIGKILL
// (killGroup).
func ignoreStopSignals() {
	signal.Ignore(syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
}

// preferOOMKill asks the kernel to pick this process first when memory
// runs short in the service's cgroup, so a script's appetite costs the
// script and not the gateway. Best effort: a sandboxed worker cannot
// reach /proc, and the gateway sets it for the child instead
// (preferOOMKillChild).
func preferOOMKill() {
	_ = os.WriteFile("/proc/self/oom_score_adj", []byte("1000"), 0)
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
