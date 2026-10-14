//go:build !linux || !(arm64 || amd64)

package codemode

import (
	"errors"
	"os/exec"
	"runtime"
)

// The worker sandbox is implemented for linux/arm64 and linux/amd64 only
// (sandbox_linux.go). Elsewhere SandboxAuto runs the worker with its
// resource caps only and SandboxRequired refuses to start scripts.

func probeSandboxSupport() sandboxSupport {
	reason := "the worker sandbox is implemented for linux/arm64 and linux/amd64 only (this is " +
		runtime.GOOS + "/" + runtime.GOARCH + ")"
	return sandboxSupport{reason: reason, seccompReason: reason}
}

// startSandboxed is never reached: probeSandboxSupport reports no Landlock.
func startSandboxed(*exec.Cmd, string, sandboxSupport) error {
	return errors.New("no worker sandbox on this platform")
}

// preferOOMKillChild: an unconfined worker marks itself (preferOOMKill).
func preferOOMKillChild(int) {}
