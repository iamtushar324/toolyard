//go:build !unix

package codemode

import (
	"errors"
	"os/exec"
	"time"
)

// Without rlimits there is no hard memory cap; the worker refuses to run
// rather than run unbounded.
func applyMemoryCap(int) error { return errors.New("no memory cap available on this platform") }

func applyCPUCap(time.Duration) {}

func ownProcessGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
