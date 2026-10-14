//go:build linux && arm64

package codemode

import "golang.org/x/sys/unix"

// seccompAuditArch is the only system call convention a worker may use.
// An AArch32 task reports AUDIT_ARCH_ARM and uses another syscall table,
// where the deny-list's numbers would name other calls.
const seccompAuditArch = unix.AUDIT_ARCH_AARCH64

// seccompABIChecks runs with the syscall number loaded. arm64 has one
// 64-bit table, so there is nothing more to refuse.
func seccompABIChecks() []unix.SockFilter { return nil }

// seccompArchDenied adds to deniedSyscalls the calls only this
// architecture has. arm64's table has no legacy chmod, chown, lchown,
// utime, utimes or futimesat; the *at forms in deniedSyscalls cover them.
var seccompArchDenied []uint32
