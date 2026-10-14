//go:build linux && (arm64 || amd64)

package codemode

import (
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The worker sandbox on Linux.
//
// Landlock (landlock_restrict_self), no_new_privs (prctl) and a seccomp
// filter all apply to the calling thread only, and a child created by
// fork or clone inherits them from the thread that created it. The worker
// cannot confine itself: by the time its main function runs, the Go
// runtime has started other threads that would stay unrestricted. Nor can
// the gateway confine all its threads at once: syscall.AllThreadsSyscall
// returns ENOTSUP in a cgo binary, and the gateway links SQLite through
// cgo.
//
// So each worker is started from a thread of the gateway that exists for
// that one start (onConfinedThread): a goroutine locks itself to its OS
// thread and never unlocks, restricts that thread, and calls cmd.Start.
// os/exec reaches syscall.forkExec, whose forkAndExecInChild1 issues the
// clone (or clone3) as a raw system call on the calling thread, so the
// child is created by, and inherits from, the restricted thread. The
// goroutine then returns still locked, and the Go runtime ends the thread
// rather than hand it to another goroutine. While the goroutine holds the
// lock, any new thread the runtime needs is created by its template
// thread, never cloned from the restricted one.
//
// The rules, built on every start so that a binary replaced on disk is
// picked up:
//
//   - Landlock handles every filesystem right the kernel's ABI knows and
//     grants only: execute and read on the worker binary and on its ELF
//     interpreter, and read on /usr/lib, /usr/lib64, /usr/local/lib,
//     /lib, /lib64 (those that exist) and /etc/ld.so.cache. Landlock
//     governs opening, creating, removing, renaming and linking files;
//     path lookup, stat, readlink and chdir stay possible everywhere. On
//     ABI 4 and later it also handles TCP bind and connect, with nothing
//     allowed; on ABI 6 and later it scopes abstract Unix sockets and
//     signals to the worker's own domain.
//   - The seccomp filter (workerSeccompFilter) returns ENOSYS for clone3;
//     EPERM for clone with a new-namespace flag or CLONE_PARENT, for
//     prlimit64 on any pid but 0 (the caller), for the calls in
//     deniedSyscalls and seccompArchDenied, and for any call made through
//     another system call convention; and allows everything else. Landlock
//     does not govern calls that act on another process of the same user
//     (resource limits, scheduling) or that change a file's metadata
//     (mode, owner, times, extended attributes, truncation below ABI 3);
//     the filter refuses those.
//
// The Go runtime in the worker does not need anything this takes away.
// It reads /sys/kernel/mm/transparent_hugepage/hpage_pmd_size and, for
// the default GOMAXPROCS, /proc/self/cgroup and /proc/self/mountinfo;
// runtime.getHugePageSize and internal/runtime/cgroup treat a failed open
// as "no huge pages" and "no cgroup limit" (GOMAXPROCS is set explicitly
// anyway). A worker with no zoneinfo uses UTC. Nothing in it opens a
// socket: it talks to the gateway over the stdin and stdout pipes it is
// started with. Its threads come from clone without namespace flags,
// either from the Go runtime directly or through glibc's pthread_create,
// which tries clone3 first and falls back to clone on ENOSYS.
//
// Whether the kernel offers Landlock and seccomp is asked of a child
// process (probeSandboxSupport), not tried in the gateway: a seccomp
// filter a service manager installed kills the whole process on a
// forbidden call, and only the probe would die. systemd's
// SystemCallFilter=@system-service allows the Landlock calls only where
// its @default set includes @sandbox (systemd 259 does; @sandbox was
// added in 254, and older versions have no such group).

// sandboxProbeEnv makes any binary that links this package answer the
// sandbox probe from its init and exit, before main or TestMain runs.
const sandboxProbeEnv = "TOOLYARD_CODEMODE_SANDBOX_PROBE"

func init() {
	if os.Getenv(sandboxProbeEnv) == "1" {
		os.Exit(sandboxProbeChild(os.Stdout))
	}
}

// probeTimeout bounds the sandbox probe.
const probeTimeout = 10 * time.Second

// probeSandboxSupport runs this binary as the sandbox probe and reads
// what it could do. A step the probe began but never reported was killed
// by a seccomp filter the gateway itself runs under.
func probeSandboxSupport() sandboxSupport {
	exe, err := os.Executable()
	if err != nil {
		return sandboxSupport{reason: "the sandbox probe could not find this binary: " + err.Error()}
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = []string{sandboxProbeEnv + "=1"}
	cmd.Dir = "/"
	stdout := &cappedBuffer{max: 4 << 10}
	stderr := &cappedBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()
	return parseSandboxProbe(stdout.String(), runErr, stderr.String())
}

// sandboxProbeChild tries, in order, every confinement call a worker start
// makes, on this (the main) thread, and prints one "step <name>" line
// before each and one "<name> <result>" line after. It always exits 0;
// the process is gone a moment later.
func sandboxProbeChild(w io.Writer) int {
	say := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	say("step landlock")
	abi, err := landlockABI()
	if err != nil {
		say("landlock error %s", errnoText(err))
	} else {
		say("landlock %d", abi)
	}
	say("step no_new_privs")
	if err := setNoNewPrivs(); err != nil {
		say("no_new_privs error %s", errnoText(err))
		return 0
	}
	say("no_new_privs ok")
	if abi > 0 {
		say("step restrict")
		if err := probeRestrict(abi); err != nil {
			say("restrict error %s", errnoText(err))
		} else {
			say("restrict ok")
		}
	}
	// The worker's own filter: this also proves the kernel accepts it.
	say("step seccomp")
	if err := installSeccomp(workerSeccompFilter); err != nil {
		say("seccomp error %s", errnoText(err))
	} else {
		say("seccomp ok")
	}
	return 0
}

// probeRestrict exercises landlock_create_ruleset, landlock_add_rule and
// landlock_restrict_self with a ruleset that leaves the probe able to
// read everything.
func probeRestrict(abi int) error {
	fd, err := createRuleset(abi)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := addPathRule(fd, "/", unix.LANDLOCK_ACCESS_FS_READ_FILE|unix.LANDLOCK_ACCESS_FS_READ_DIR, false); err != nil {
		return err
	}
	return restrictSelf(fd)
}

// parseSandboxProbe turns the probe's output into a sandboxSupport.
func parseSandboxProbe(out string, runErr error, stderr string) sandboxSupport {
	results := map[string]string{}
	pending := ""
	var s sandboxSupport
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if key == "step" {
			pending = value
			continue
		}
		results[key] = value
		if key == pending {
			pending = ""
		}
	}
	// killedBy names the call a seccomp filter on the gateway killed the
	// probe on, if that is what happened.
	killedBy := ""
	var exitErr *exec.ExitError
	if pending != "" && errors.As(runErr, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGSYS {
			killedBy = pending
		}
	}
	// A probe that ran to completion, or that a seccomp filter killed,
	// gave the kernel's answer; anything else (a timeout, a failed fork,
	// no output) may go away, and is probed again later.
	s.definite = killedBy != "" || (runErr == nil && results["landlock"] != "")
	// blocked names the call and the systemd SystemCallFilter= group that
	// holds it.
	blocked := func(what, group string) string {
		return "a seccomp filter on the gateway process kills " + what +
			" (under systemd SystemCallFilter=, allow " + group + ")"
	}
	failed := func() string {
		detail := "it reported nothing"
		if runErr != nil {
			detail = runErr.Error()
		}
		if line := firstLine(stderr); line != "" {
			detail += ": " + line
		}
		return "the sandbox probe failed (" + detail + ")"
	}

	switch v := results["landlock"]; {
	case killedBy == "landlock":
		s.reason = blocked("landlock_create_ruleset", "@sandbox")
	case v == "":
		s.reason = failed()
	case strings.HasPrefix(v, "error "):
		switch name := strings.TrimPrefix(v, "error "); name {
		case "ENOSYS":
			s.reason = "the kernel has no Landlock support (landlock_create_ruleset: ENOSYS)"
		case "EOPNOTSUPP":
			s.reason = "Landlock is built into the kernel but not enabled (EOPNOTSUPP; add landlock to the lsm= boot parameter)"
		default:
			s.reason = "landlock_create_ruleset failed: " + name
		}
	default:
		abi, err := strconv.Atoi(v)
		if err != nil || abi <= 0 {
			s.reason = "the sandbox probe reported Landlock ABI " + strconv.Quote(v)
			break
		}
		s.abi = abi
		switch {
		case killedBy == "no_new_privs":
			s.abi, s.reason = 0, blocked("prctl(PR_SET_NO_NEW_PRIVS)", "@process")
		case results["no_new_privs"] != "ok":
			s.abi, s.reason = 0, "prctl(PR_SET_NO_NEW_PRIVS) failed: "+strings.TrimPrefix(results["no_new_privs"], "error ")
		case killedBy == "restrict":
			s.abi, s.reason = 0, blocked("landlock_add_rule or landlock_restrict_self", "@sandbox")
		case results["restrict"] != "ok":
			detail := strings.TrimPrefix(results["restrict"], "error ")
			if detail == "" {
				detail = failed()
			}
			s.abi, s.reason = 0, fmt.Sprintf("Landlock ABI %d is present but restricting a process failed: %s", abi, detail)
		}
	}
	switch v := results["seccomp"]; {
	case v == "ok":
		s.seccomp = true
	case killedBy == "seccomp":
		s.seccompReason = blocked("prctl(PR_SET_SECCOMP)", "@process")
	case v == "":
		s.seccompReason = "not probed"
	default:
		s.seccompReason = "prctl(PR_SET_SECCOMP) failed: " + strings.TrimPrefix(v, "error ")
	}
	return s
}

// errnoText names an errno ("EACCES"), or returns the error's text.
func errnoText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	return err.Error()
}

var pidfdWarmup sync.Once

// startSandboxed starts cmd from a confined, single-use thread: no new
// privileges, the Landlock ruleset when support has an ABI, the seccomp
// filter when support has one.
func startSandboxed(cmd *exec.Cmd, exe string, support sandboxSupport) error {
	ruleset := -1
	if support.abi > 0 {
		fd, err := buildWorkerRuleset(exe, support.abi)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		ruleset = fd
	}

	// os/exec opens /dev/null inside Start for a nil Stdin, Stdout or
	// Stderr, which the confined thread can no longer do.
	if cmd.Stdin == nil || cmd.Stdout == nil || cmd.Stderr == nil {
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer null.Close()
		if cmd.Stdin == nil {
			cmd.Stdin = null
		}
		if cmd.Stdout == nil {
			cmd.Stdout = null
		}
		if cmd.Stderr == nil {
			cmd.Stderr = null
		}
	}
	// The first process start checks, once per process, whether pidfds
	// work, by sending signal 0 to the gateway itself. From the confined
	// thread that signal can be refused (signal scoping on kernels that
	// do not exempt a process's own threads), which would turn pidfds off
	// for the whole gateway. os.FindProcess runs the same check here.
	pidfdWarmup.Do(func() {
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Release()
		}
	})

	return onConfinedThread(func() error {
		if err := setNoNewPrivs(); err != nil {
			return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
		}
		if ruleset >= 0 {
			if err := restrictSelf(ruleset); err != nil {
				return fmt.Errorf("landlock_restrict_self: %w", err)
			}
		}
		if support.seccomp {
			if err := installSeccomp(workerSeccompFilter); err != nil {
				return fmt.Errorf("seccomp: %w", err)
			}
		}
		return cmd.Start()
	})
}

// onConfinedThread runs fn on an OS thread that is used for nothing else
// and ends when fn returns, so fn may restrict the thread. The goroutine
// locks itself to its thread and never unlocks: when a goroutine exits
// while locked, the runtime terminates the thread instead of reusing it.
//
// The main thread is the exception: the runtime cannot end it and parks
// it for good instead, and it is the thread other processes' signals and
// /proc lookups resolve to. If the goroutine lands there, it keeps the
// main thread locked (so no other goroutine can run on it), runs fn from
// a fresh goroutine, which therefore gets another thread, and then gives
// the main thread back untouched.
func onConfinedThread(fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if unix.Gettid() == unix.Getpid() {
			done <- onConfinedThread(fn)
			runtime.UnlockOSThread()
			return
		}
		// Deliberately never unlocked: see above.
		done <- fn()
	}()
	return <-done
}

// Landlock.

// landlockABI asks the kernel for its Landlock ABI version.
func landlockABI() (int, error) {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

// handledFSAccess is every filesystem right Landlock ABI abi knows.
func handledFSAccess(abi int) uint64 {
	rights := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		rights |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		rights |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		rights |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return rights
}

// createRuleset creates a ruleset that handles everything ABI abi can
// restrict. The attribute struct is passed whole: the kernel accepts a
// larger struct than it knows as long as the fields it does not know are
// zero, which they are below the ABI that added them.
func createRuleset(abi int) (int, error) {
	attr := unix.LandlockRulesetAttr{Access_fs: handledFSAccess(abi)}
	if abi >= 4 {
		attr.Access_net = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	}
	if abi >= 6 {
		attr.Scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	}
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if e != 0 {
		return -1, fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	return int(r), nil
}

// addPathRule allows access beneath path. An optional path that does not
// exist is skipped.
func addPathRule(ruleset int, path string, access uint64, optional bool) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		if optional && errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer unix.Close(fd)
	// The kernel reads the packed 12-byte struct landlock_path_beneath_attr;
	// the Go struct has the same first 12 bytes plus padding.
	attr := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&attr)), 0, 0, 0)
	if e != 0 {
		return fmt.Errorf("landlock_add_rule %s: %w", path, e)
	}
	return nil
}

// buildWorkerRuleset is the worker's whole filesystem view.
func buildWorkerRuleset(exe string, abi int) (fd int, err error) {
	const (
		execRead = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE
		readFile = unix.LANDLOCK_ACCESS_FS_READ_FILE
		readTree = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	)
	interp, err := elfInterpreter(exe)
	if err != nil {
		return -1, err
	}
	fd, err = createRuleset(abi)
	if err != nil {
		return -1, err
	}
	defer func() {
		if err != nil {
			unix.Close(fd)
			fd = -1
		}
	}()
	if err := addPathRule(fd, exe, execRead, false); err != nil {
		return fd, err
	}
	if interp != "" {
		if err := addPathRule(fd, interp, execRead, false); err != nil {
			return fd, err
		}
	}
	for _, dir := range []string{"/usr/lib", "/usr/lib64", "/usr/local/lib", "/lib", "/lib64"} {
		if err := addPathRule(fd, dir, readTree, true); err != nil {
			return fd, err
		}
	}
	if err := addPathRule(fd, "/etc/ld.so.cache", readFile, true); err != nil {
		return fd, err
	}
	return fd, nil
}

// elfInterpreter returns the PT_INTERP path of the ELF executable at path
// (the dynamic loader the kernel runs for it), or "" for a static binary.
func elfInterpreter(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(p.Open(), 4096))
		if err != nil {
			return "", fmt.Errorf("read the ELF interpreter of %s: %w", path, err)
		}
		return string(bytes.TrimRight(b, "\x00")), nil
	}
	return "", nil
}

// restrictSelf confines the calling thread to ruleset.
func restrictSelf(ruleset int) error {
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); e != 0 {
		return e
	}
	return nil
}

// setNoNewPrivs sets no_new_privs on the calling thread, which lets an
// unprivileged thread restrict itself with Landlock and seccomp.
func setNoNewPrivs() error {
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

// seccomp.

// Offsets in struct seccomp_data (include/uapi/linux/seccomp.h):
// int nr; __u32 arch; __u64 instruction_pointer; __u64 args[6].
// seccompDataArg0Low is the low 32 bits of args[0]: arm64 and amd64 are
// little-endian.
const (
	seccompDataNr      = 0
	seccompDataArch    = 4
	seccompDataArg0Low = 16
)

// seccompDeny is the verdict for a refused call.
//
// A call through a foreign convention (seccompAuditArch,
// seccompABIChecks) gets EPERM too rather than SECCOMP_RET_KILL_PROCESS:
// either way it does nothing, the worker never makes one, and EPERM keeps
// a single failure mode, where a kill would surface as SIGSYS, which
// workerDied reads as a service manager's filter refusing setrlimit.
const seccompDeny = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)

// seccompNoSys answers clone3, whose flags sit in memory a filter cannot
// read. ENOSYS reads as "this kernel has no clone3", and glibc's thread
// creation then falls back to clone, whose flags the filter checks. The
// Go runtime's threads and os/exec's fork use clone already.
const seccompNoSys = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)

// cloneDeniedFlags are the clone flags that create a namespace, and
// CLONE_PARENT, which would make the new process the gateway's child. All
// sit in the low 32 bits of the flags argument. The runtime's own clones
// (threads: CLONE_VM|CLONE_FS|CLONE_FILES|CLONE_SIGHAND|CLONE_SYSVSEM|
// CLONE_THREAD; os/exec: CLONE_VFORK|CLONE_VM|CLONE_PIDFD) carry none.
const cloneDeniedFlags = unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC |
	unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP |
	unix.CLONE_PARENT

// deniedSyscalls are refused with EPERM on every architecture, with
// seccompArchDenied. The worker uses none of them, and each opens kernel
// surface or reach a script has no use for: sockets (the worker talks
// over its pipes), io_uring (a second syscall path), ptrace and
// process_vm_* (other processes' memory), bpf, perf_event_open,
// userfaultfd, the kernel key store, unshare and setns (namespaces; clone
// and clone3 are handled in buildSeccompFilter), mounting, old API and
// new, scheduling another process of the same user (setpriority,
// sched_set*, ioprio_set; prlimit64 is handled in buildSeccompFilter),
// and changing file metadata, which Landlock does not govern.
var deniedSyscalls = []uint32{
	unix.SYS_SOCKET,
	unix.SYS_SOCKETPAIR,
	unix.SYS_IO_URING_SETUP,
	unix.SYS_IO_URING_ENTER,
	unix.SYS_IO_URING_REGISTER,
	unix.SYS_PTRACE,
	unix.SYS_PROCESS_VM_READV,
	unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_BPF,
	unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_USERFAULTFD,
	unix.SYS_ADD_KEY,
	unix.SYS_REQUEST_KEY,
	unix.SYS_KEYCTL,
	unix.SYS_UNSHARE,
	unix.SYS_SETNS,
	unix.SYS_MOUNT,
	unix.SYS_UMOUNT2,
	unix.SYS_PIVOT_ROOT,
	unix.SYS_OPEN_TREE,
	unix.SYS_OPEN_TREE_ATTR,
	unix.SYS_MOVE_MOUNT,
	unix.SYS_FSOPEN,
	unix.SYS_FSCONFIG,
	unix.SYS_FSMOUNT,
	unix.SYS_FSPICK,
	unix.SYS_MOUNT_SETATTR,
	unix.SYS_SETPRIORITY,
	unix.SYS_SCHED_SETSCHEDULER,
	unix.SYS_SCHED_SETPARAM,
	unix.SYS_SCHED_SETATTR,
	unix.SYS_SCHED_SETAFFINITY,
	unix.SYS_IOPRIO_SET,
	unix.SYS_FCHMOD,
	unix.SYS_FCHMODAT,
	unix.SYS_FCHMODAT2,
	unix.SYS_FCHOWN,
	unix.SYS_FCHOWNAT,
	unix.SYS_UTIMENSAT,
	unix.SYS_TRUNCATE,
	unix.SYS_SETXATTR,
	unix.SYS_LSETXATTR,
	unix.SYS_FSETXATTR,
	unix.SYS_SETXATTRAT,
	unix.SYS_REMOVEXATTR,
	unix.SYS_LREMOVEXATTR,
	unix.SYS_FREMOVEXATTR,
	unix.SYS_REMOVEXATTRAT,
}

// workerSeccompFilter is the classic BPF program every worker runs under.
var workerSeccompFilter = buildSeccompFilter()

func bpfStmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

// buildSeccompFilter: refuse any call made through a convention other
// than the native one; answer clone3 with ENOSYS; refuse clone with a
// denied flag and allow any other clone; refuse prlimit64 on any pid but
// 0 and allow it on 0; refuse each denied call; allow the rest.
func buildSeccompFilter() []unix.SockFilter {
	f := []unix.SockFilter{
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArch),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, seccompAuditArch, 1, 0),
		bpfStmt(unix.BPF_RET|unix.BPF_K, seccompDeny),
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataNr),
	}
	f = append(f, seccompABIChecks()...)
	f = append(f,
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.SYS_CLONE3, 0, 1),
		bpfStmt(unix.BPF_RET|unix.BPF_K, seccompNoSys),
		// Not clone: skip the next four, with the number still loaded.
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.SYS_CLONE, 0, 4),
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArg0Low),
		bpfJump(unix.BPF_JMP|unix.BPF_JSET|unix.BPF_K, cloneDeniedFlags, 0, 1),
		bpfStmt(unix.BPF_RET|unix.BPF_K, seccompDeny),
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
		// prlimit64(pid, ...): pid 0 is the caller itself, which the
		// worker's own setrlimit and os/exec's child before exec use;
		// another pid would reach the gateway. The kernel reads pid as a
		// 32-bit pid_t. Not prlimit64: skip the next four.
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.SYS_PRLIMIT64, 0, 4),
		bpfStmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArg0Low),
		bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, 0, 1, 0),
		bpfStmt(unix.BPF_RET|unix.BPF_K, seccompDeny),
		bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
	)
	for _, nr := range append(append([]uint32(nil), deniedSyscalls...), seccompArchDenied...) {
		f = append(f,
			bpfJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, nr, 0, 1),
			bpfStmt(unix.BPF_RET|unix.BPF_K, seccompDeny),
		)
	}
	return append(f, bpfStmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
}

// installSeccomp attaches filter to the calling thread. It goes through
// prctl rather than seccomp(2): prctl is in systemd's @process group,
// which every @system-service allows, while seccomp(2) is in @sandbox,
// which older systemd versions leave out. no_new_privs must already be
// set.
func installSeccomp(filter []unix.SockFilter) error {
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, e := unix.Syscall6(unix.SYS_PRCTL, unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER,
		uintptr(unsafe.Pointer(&prog)), 0, 0, 0)
	runtime.KeepAlive(filter)
	if e != 0 {
		return e
	}
	return nil
}

// preferOOMKillChild marks a worker first in line for the OOM killer. The
// worker does the same for itself (preferOOMKill), which a confined worker
// cannot: /proc is outside its ruleset. Best effort.
func preferOOMKillChild(pid int) {
	_ = os.WriteFile("/proc/"+strconv.Itoa(pid)+"/oom_score_adj", []byte("1000"), 0)
}
