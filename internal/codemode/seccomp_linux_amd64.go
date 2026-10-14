//go:build linux && amd64

package codemode

import "golang.org/x/sys/unix"

// seccompAuditArch is the only system call convention a worker may use.
// i386 calls (int 0x80) report AUDIT_ARCH_I386 and use another syscall
// table, where the deny-list's numbers would name other calls.
const seccompAuditArch = unix.AUDIT_ARCH_X86_64

// x32SyscallBit marks an x32 call, which reports AUDIT_ARCH_X86_64 too
// (__X32_SYSCALL_BIT, arch/x86/include/uapi/asm/unistd.h).
const x32SyscallBit = 0x40000000

// seccompABIChecks runs with the syscall number loaded: refuse every x32
// call, whose numbers differ from the x86-64 ones the deny-list holds.
func seccompABIChecks() []unix.SockFilter {
	return []unix.SockFilter{
		bpfJump(unix.BPF_JMP|unix.BPF_JGE|unix.BPF_K, x32SyscallBit, 0, 1),
		bpfStmt(unix.BPF_RET|unix.BPF_K, seccompDeny),
	}
}

// seccompArchDenied adds to deniedSyscalls the legacy metadata calls
// only x86-64's table has.
var seccompArchDenied = []uint32{
	unix.SYS_CHMOD,
	unix.SYS_CHOWN,
	unix.SYS_LCHOWN,
	unix.SYS_UTIME,
	unix.SYS_UTIMES,
	unix.SYS_FUTIMESAT,
}
