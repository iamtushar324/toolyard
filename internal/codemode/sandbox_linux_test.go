//go:build linux && (arm64 || amd64)

package codemode

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The sandbox tests start this test binary as a probe (TestMain, seeing
// the marker variable, runs it instead of the tests) that tries what a
// confined worker must not do and prints each outcome as JSON. Each test
// runs the probe unconfined first, to show the action works without the
// sandbox, then confined.
const sandboxTestProbeEnv = "TOOLYARD_CODEMODE_TEST_SANDBOX_PROBE"

// sandboxMinABIEnv, when set, turns a missing sandbox into a failure
// instead of a skip: the tests fail when the probed Landlock ABI is below
// its value or no seccomp filter can be installed. CI sets it, so a
// sandbox that silently disappears breaks the build.
const sandboxMinABIEnv = "TOOLYARD_SANDBOX_TEST_MIN_ABI"

func init() {
	sandboxProbeMain = func() (int, bool) {
		raw := os.Getenv(sandboxTestProbeEnv)
		if raw == "" {
			return 0, false
		}
		return runSandboxTestProbe(raw), true
	}
}

// probeSpec says what the probe tries. The sandbox refuses to create
// sockets, so Landlock's TCP rules show only on sockets the parent passes
// in: fd 3 for connect, fd 4 for bind.
type probeSpec struct {
	Read        string `json:"read,omitempty"`        // read this file
	Hostname    bool   `json:"hostname,omitempty"`    // read /etc/hostname
	Create      string `json:"create,omitempty"`      // create this file
	Exec        string `json:"exec,omitempty"`        // run this program
	ConnectPort int    `json:"connectPort,omitempty"` // connect fd 3 to 127.0.0.1:port
	Bind        bool   `json:"bind,omitempty"`        // bind fd 4 to 127.0.0.1:0
	Sockets     bool   `json:"sockets,omitempty"`     // create a Unix and a TCP socket
	Signal      bool   `json:"signal,omitempty"`      // kill(getppid(), 0)
	Clone       bool   `json:"clone,omitempty"`       // clone3, and clone with CLONE_NEWUSER
	Prlimit     bool   `json:"prlimit,omitempty"`     // read RLIMIT_CPU of the parent and of itself
	Chmod       string `json:"chmod,omitempty"`       // chmod this file
}

func runSandboxTestProbe(raw string) int {
	var spec probeSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		fmt.Fprintln(os.Stderr, "probe: bad spec:", err)
		return 2
	}
	res := map[string]string{}
	if spec.Read != "" {
		_, err := os.ReadFile(spec.Read)
		res["read"] = probeOutcome(err)
	}
	if spec.Hostname {
		_, err := os.ReadFile("/etc/hostname")
		res["hostname"] = probeOutcome(err)
	}
	if spec.Create != "" {
		f, err := os.OpenFile(spec.Create, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()
		}
		res["create"] = probeOutcome(err)
	}
	if spec.Exec != "" {
		// ForkExec reports the child's execve error itself, so the
		// outcome is the exec's, not that of opening /dev/null.
		pid, err := syscall.ForkExec(spec.Exec, []string{spec.Exec}, &syscall.ProcAttr{})
		if err == nil {
			var ws syscall.WaitStatus
			if _, err = syscall.Wait4(pid, &ws, 0, nil); err == nil && ws.ExitStatus() != 0 {
				err = fmt.Errorf("exit status %d", ws.ExitStatus())
			}
		}
		res["exec"] = probeOutcome(err)
	}
	if spec.ConnectPort != 0 {
		res["connect"] = probeOutcome(unix.Connect(3, &unix.SockaddrInet4{Port: spec.ConnectPort, Addr: [4]byte{127, 0, 0, 1}}))
	}
	if spec.Bind {
		res["bind"] = probeOutcome(unix.Bind(4, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
	}
	if spec.Sockets {
		for name, family := range map[string]int{"unix_socket": unix.AF_UNIX, "tcp_socket": unix.AF_INET} {
			fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err == nil {
				unix.Close(fd)
			}
			res[name] = probeOutcome(err)
		}
	}
	if spec.Signal {
		res["signal"] = probeOutcome(unix.Kill(unix.Getppid(), 0))
	}
	if spec.Clone {
		// Both calls are invalid as made, so the kernel refuses them with
		// EINVAL and creates nothing: clone3 with a zero-sized argument,
		// and CLONE_NEWUSER with CLONE_FS (copy_process rejects that pair
		// before anything else). Confined, the filter answers first.
		_, _, e := unix.Syscall(unix.SYS_CLONE3, 0, 0, 0)
		res["clone3"] = probeOutcome(errnoErr(e))
		_, _, e = unix.RawSyscall(unix.SYS_CLONE, unix.CLONE_NEWUSER|unix.CLONE_FS, 0, 0)
		res["clone_newuser"] = probeOutcome(errnoErr(e))
	}
	if spec.Prlimit {
		// Reads only (no new limit), so the unconfined run changes nothing.
		var lim unix.Rlimit
		res["prlimit_parent"] = probeOutcome(unix.Prlimit(unix.Getppid(), unix.RLIMIT_CPU, nil, &lim))
		res["prlimit_self"] = probeOutcome(unix.Prlimit(0, unix.RLIMIT_CPU, nil, &lim))
	}
	if spec.Chmod != "" {
		res["chmod"] = probeOutcome(unix.Fchmodat(unix.AT_FDCWD, spec.Chmod, 0o600, 0))
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		return 1
	}
	return 0
}

// errnoErr is e as an error, nil for 0.
func errnoErr(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}

// probeOutcome is "ok", an errno name ("EACCES"), or an error's text.
func probeOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	return errnoText(err)
}

// runProbe starts the probe as mode says and returns what it reported.
func runProbe(t *testing.T, mode SandboxMode, spec probeSpec, extra ...*os.File) map[string]string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = []string{sandboxTestProbeEnv + "=" + string(raw)}
	cmd.Dir = "/"
	cmd.ExtraFiles = extra
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := startConfined(cmd, exe, mode); err != nil {
		t.Fatalf("start the probe (sandbox %s): %v", mode, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("probe (sandbox %s): %v\n%s", mode, err, stderr.String())
	}
	res := map[string]string{}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("probe (sandbox %s) printed %q: %v\n%s", mode, stdout.String(), err, stderr.String())
	}
	return res
}

func expectOutcomes(t *testing.T, label string, got map[string]string, want string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if got[k] != want {
			t.Errorf("%s: %s = %q, want %q (all: %v)", label, k, got[k], want, got)
		}
	}
}

// probedSandbox logs what the kernel offers and, under sandboxMinABIEnv,
// fails the test when that is less than the floor.
func probedSandbox(t *testing.T) sandboxSupport {
	t.Helper()
	s := sandboxSupported()
	t.Logf("landlock abi=%d seccomp=%v", s.abi, s.seccomp)
	if v := os.Getenv(sandboxMinABIEnv); v != "" {
		floor, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("%s=%q: %v", sandboxMinABIEnv, v, err)
		}
		if s.abi < floor {
			t.Fatalf("landlock abi=%d is below %s=%d: %s", s.abi, sandboxMinABIEnv, floor, s.reason)
		}
		if !s.seccomp {
			t.Fatalf("no seccomp filter, which %s requires: %s", sandboxMinABIEnv, s.seccompReason)
		}
	}
	return s
}

// landlockOrSkip skips the test when the kernel has no Landlock.
func landlockOrSkip(t *testing.T) sandboxSupport {
	t.Helper()
	s := probedSandbox(t)
	if s.abi == 0 {
		t.Skipf("landlock abi=0: %s", s.reason)
	}
	return s
}

// seccompOrSkip skips the test when no seccomp filter can be installed.
func seccompOrSkip(t *testing.T) sandboxSupport {
	t.Helper()
	s := probedSandbox(t)
	if !s.seccomp {
		t.Skipf("no seccomp filter: %s", s.seccompReason)
	}
	return s
}

// simulateSupport makes the sandbox see s for the rest of the test.
func simulateSupport(t *testing.T, s sandboxSupport) {
	t.Helper()
	s.definite = true
	prev := supportOverride.Swap(&s)
	t.Cleanup(func() { supportOverride.Store(prev) })
}

// simulateNoLandlock makes the sandbox see a kernel with neither Landlock
// nor seccomp for the rest of the test.
func simulateNoLandlock(t *testing.T) {
	t.Helper()
	simulateSupport(t, sandboxSupport{reason: "simulated: no Landlock", seccompReason: "simulated"})
}

// tcpSockets makes the two TCP sockets the probe uses as fds 3 and 4.
func tcpSockets(t *testing.T) []*os.File {
	t.Helper()
	var files []*os.File
	for i := 0; i < 2; i++ {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		f := os.NewFile(uintptr(fd), "tcp")
		t.Cleanup(func() { f.Close() })
		files = append(files, f)
	}
	return files
}

func TestWorkerSandboxDeniesFilesystem(t *testing.T) {
	s := landlockOrSkip(t)
	status := sandboxStatusText(SandboxAuto, s)
	t.Logf("sandbox status: %s", status)
	if want := fmt.Sprintf("landlock-abi-%d", s.abi); !strings.HasPrefix(status, want) {
		t.Fatalf("status %q, want it to start with %q", status, want)
	}
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("parent data"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := func(create string) probeSpec {
		return probeSpec{Read: secret, Hostname: true, Create: filepath.Join(dir, create), Exec: "/bin/true"}
	}
	keys := []string{"read", "hostname", "create", "exec"}
	expectOutcomes(t, "unconfined", runProbe(t, SandboxOff, spec("open")), "ok", keys...)
	expectOutcomes(t, "confined", runProbe(t, SandboxAuto, spec("shut")), "EACCES", keys...)
	if _, err := os.Stat(filepath.Join(dir, "shut")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the confined probe created a file (stat: %v)", err)
	}
}

func TestWorkerSandboxDeniesNetwork(t *testing.T) {
	s := landlockOrSkip(t)
	if s.abi < 4 && !s.seccomp {
		t.Skipf("landlock abi=%d < 4 and no seccomp filter (%s): nothing confines the network", s.abi, s.seccompReason)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	spec := probeSpec{ConnectPort: ln.Addr().(*net.TCPAddr).Port, Bind: true, Sockets: true}
	open := runProbe(t, SandboxOff, spec, tcpSockets(t)...)
	expectOutcomes(t, "unconfined", open, "ok", "connect", "bind", "unix_socket", "tcp_socket")
	shut := runProbe(t, SandboxAuto, spec, tcpSockets(t)...)
	if s.seccomp {
		expectOutcomes(t, "confined", shut, "EPERM", "unix_socket", "tcp_socket")
	} else {
		t.Logf("no seccomp filter (%s): socket creation not checked", s.seccompReason)
	}
	if s.abi >= 4 {
		expectOutcomes(t, "confined", shut, "EACCES", "connect", "bind")
	} else {
		t.Logf("landlock abi=%d < 4: TCP bind and connect are not covered by Landlock; seccomp refuses the sockets instead", s.abi)
	}
}

func TestWorkerSandboxScopesSignals(t *testing.T) {
	s := landlockOrSkip(t)
	if s.abi < 6 {
		t.Skipf("landlock abi=%d < 6: no signal scoping", s.abi)
	}
	expectOutcomes(t, "unconfined", runProbe(t, SandboxOff, probeSpec{Signal: true}), "ok", "signal")
	expectOutcomes(t, "confined", runProbe(t, SandboxAuto, probeSpec{Signal: true}), "EPERM", "signal")
}

func TestWorkerSandboxDegradesWhenUnavailable(t *testing.T) {
	probedSandbox(t)
	simulateNoLandlock(t)
	rt := workerRuntime(newFake(), Limits{}, nil)
	if got, want := SandboxStatus(), "none: simulated: no Landlock"; got != want {
		t.Fatalf("SandboxStatus = %q, want %q", got, want)
	}
	text, failed := rt.ExecuteToolCode(context.Background(), "print(\"hi\")\nresult = 1", "")
	if failed || !strings.Contains(text, "Execution completed successfully") {
		t.Fatalf("degraded run (failed=%v):\n%s", failed, text)
	}
	// The worker runs with its resource caps only: it reads what the
	// gateway can.
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("parent data"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectOutcomes(t, "degraded", runProbe(t, SandboxAuto, probeSpec{Read: secret}), "ok", "read")
}

// TestWorkerSandboxSeccompOnlyWithoutLandlock: in auto mode a kernel
// without Landlock still gets no_new_privs and the seccomp filter.
func TestWorkerSandboxSeccompOnlyWithoutLandlock(t *testing.T) {
	seccompOrSkip(t)
	simulateSupport(t, sandboxSupport{reason: "simulated: no Landlock", seccomp: true})
	rt := workerRuntime(newFake(), Limits{}, nil)
	if got, want := SandboxStatus(), "seccomp-only: simulated: no Landlock"; got != want {
		t.Fatalf("SandboxStatus = %q, want %q", got, want)
	}
	text, failed := rt.ExecuteToolCode(context.Background(), "print(\"hi\")\nresult = 1", "")
	if failed || !strings.Contains(text, "Execution completed successfully") {
		t.Fatalf("seccomp-only run (failed=%v):\n%s", failed, text)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("parent data"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runProbe(t, SandboxAuto, probeSpec{Read: secret, Sockets: true})
	expectOutcomes(t, "seccomp only", got, "ok", "read")
	expectOutcomes(t, "seccomp only", got, "EPERM", "unix_socket", "tcp_socket")
}

func TestWorkerSandboxRequiredRefuses(t *testing.T) {
	actual := probedSandbox(t)
	t.Run("without landlock", func(t *testing.T) {
		simulateNoLandlock(t)
		rt := workerRuntime(newFake(), Limits{}, func(w *Worker) { w.Sandbox = SandboxRequired })
		if got, want := SandboxStatus(), "none: simulated: no Landlock (sandbox required: scripts are refused)"; got != want {
			t.Fatalf("SandboxStatus = %q, want %q", got, want)
		}
		text, failed := rt.ExecuteToolCode(context.Background(), "result = 1", "")
		want := "could not start the script worker: the worker sandbox is required, but Landlock is unavailable: simulated: no Landlock"
		if !failed || !strings.Contains(text, want) {
			t.Fatalf("required without Landlock (failed=%v):\n%s", failed, text)
		}
		if pid := rt.lastPID.Load(); pid != 0 {
			t.Fatalf("a worker started anyway (pid %d)", pid)
		}
		expectClean(t, rt, 0)
	})
	t.Run("without seccomp", func(t *testing.T) {
		simulateSupport(t, sandboxSupport{abi: 6, seccompReason: "simulated: no seccomp"})
		rt := workerRuntime(newFake(), Limits{}, func(w *Worker) { w.Sandbox = SandboxRequired })
		if got, want := SandboxStatus(), "landlock-abi-6 (sandbox required: scripts are refused)"; got != want {
			t.Fatalf("SandboxStatus = %q, want %q", got, want)
		}
		text, failed := rt.ExecuteToolCode(context.Background(), "result = 1", "")
		want := "could not start the script worker: the worker sandbox is required, but no seccomp filter can be installed: simulated: no seccomp"
		if !failed || !strings.Contains(text, want) {
			t.Fatalf("required without seccomp (failed=%v):\n%s", failed, text)
		}
		if pid := rt.lastPID.Load(); pid != 0 {
			t.Fatalf("a worker started anyway (pid %d)", pid)
		}
		expectClean(t, rt, 0)
	})
	t.Run("with both", func(t *testing.T) {
		if actual.abi == 0 || !actual.seccomp {
			t.Skipf("landlock abi=%d seccomp=%v: %s %s", actual.abi, actual.seccomp, actual.reason, actual.seccompReason)
		}
		rt := workerRuntime(newFake(), Limits{}, func(w *Worker) { w.Sandbox = SandboxRequired })
		text, failed := rt.ExecuteToolCode(context.Background(), "print(\"hi\")\nresult = 1", "")
		if failed || !strings.Contains(text, "Execution completed successfully") {
			t.Fatalf("required with Landlock (failed=%v):\n%s", failed, text)
		}
	})
}

// TestWorkerSandboxProbeParsing: the probe's report, including a probe a
// seccomp filter on the gateway killed mid-step, as under a systemd
// SystemCallFilter= without the @sandbox calls.
func TestWorkerSandboxProbeParsing(t *testing.T) {
	probedSandbox(t)
	full := "step landlock\nlandlock 6\nstep no_new_privs\nno_new_privs ok\nstep restrict\nrestrict ok\nstep seccomp\nseccomp ok\n"
	if s := parseSandboxProbe(full, nil, ""); s.abi != 6 || !s.seccomp || s.reason != "" || !s.definite {
		t.Fatalf("full support parsed as %+v", s)
	}
	none := "step landlock\nlandlock error ENOSYS\nstep no_new_privs\nno_new_privs ok\nstep seccomp\nseccomp ok\n"
	if s := parseSandboxProbe(none, nil, ""); s.abi != 0 || !s.seccomp || !strings.Contains(s.reason, "no Landlock support") || !s.definite {
		t.Fatalf("no Landlock parsed as %+v", s)
	}
	// A probe that never ran is not an answer, and is probed again.
	if s := parseSandboxProbe("", errors.New("fork/exec: resource temporarily unavailable"), ""); s.abi != 0 || s.definite ||
		!strings.Contains(s.reason, "the sandbox probe failed (fork/exec: resource temporarily unavailable)") {
		t.Fatalf("failed probe parsed as %+v", s)
	}
	// A real SIGSYS death, from a shell standing in for the probe.
	cmd := exec.Command("/bin/sh", "-c", "echo step landlock; kill -SYS $$")
	out, err := cmd.Output()
	s := parseSandboxProbe(string(out), err, "")
	if s.abi != 0 || !s.definite || !strings.Contains(s.reason, "a seccomp filter on the gateway process kills landlock_create_ruleset (under systemd SystemCallFilter=, allow @sandbox)") {
		t.Fatalf("killed probe (err %v) parsed as %+v", err, s)
	}
	cmd = exec.Command("/bin/sh", "-c", "echo step landlock; echo landlock 7; echo step no_new_privs; kill -SYS $$")
	out, err = cmd.Output()
	if s := parseSandboxProbe(string(out), err, ""); s.abi != 0 || !s.definite ||
		!strings.Contains(s.reason, "kills prctl(PR_SET_NO_NEW_PRIVS) (under systemd SystemCallFilter=, allow @process)") {
		t.Fatalf("probe killed at prctl (err %v) parsed as %+v", err, s)
	}
	if got := sandboxStatusText(SandboxAuto, s); !strings.HasPrefix(got, "none: a seccomp filter") {
		t.Fatalf("status for a killed probe: %q", got)
	}
	for _, c := range []struct {
		mode SandboxMode
		s    sandboxSupport
		want string
	}{
		{SandboxAuto, sandboxSupport{abi: 8, seccomp: true}, "landlock-abi-8+seccomp"},
		{SandboxAuto, sandboxSupport{abi: 6}, "landlock-abi-6"},
		{SandboxRequired, sandboxSupport{abi: 6}, "landlock-abi-6 (sandbox required: scripts are refused)"},
		{SandboxRequired, sandboxSupport{abi: 7, seccomp: true}, "landlock-abi-7+seccomp"},
		{SandboxAuto, sandboxSupport{abi: 5, seccomp: true}, "landlock-abi-5+seccomp (signals unscoped)"},
		{SandboxRequired, sandboxSupport{abi: 4, seccomp: true}, "landlock-abi-4+seccomp (signals unscoped)"},
		{SandboxOff, sandboxSupport{abi: 8, seccomp: true}, "off"},
		{SandboxAuto, sandboxSupport{reason: "r"}, "none: r"},
		{SandboxRequired, sandboxSupport{reason: "r"}, "none: r (sandbox required: scripts are refused)"},
		{SandboxAuto, sandboxSupport{reason: "r", seccomp: true}, "seccomp-only: r"},
		{SandboxRequired, sandboxSupport{reason: "r", seccomp: true}, "seccomp-only: r (sandbox required: scripts are refused)"},
	} {
		if got := sandboxStatusText(c.mode, c.s); got != c.want {
			t.Errorf("status(%s, %+v) = %q, want %q", c.mode, c.s, got, c.want)
		}
	}
}

func TestWorkerSandboxParseMode(t *testing.T) {
	probedSandbox(t)
	for in, want := range map[string]SandboxMode{"": SandboxAuto, "auto": SandboxAuto, " Off ": SandboxOff, "REQUIRED": SandboxRequired} {
		if got, err := ParseSandboxMode(in); err != nil || got != want {
			t.Errorf("ParseSandboxMode(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	if got, err := ParseSandboxMode("strict"); err == nil || got != SandboxAuto {
		t.Errorf("ParseSandboxMode(\"strict\") = %s, %v; want auto and an error", got, err)
	}
}

func TestWorkerSandboxDeniesNamespaces(t *testing.T) {
	seccompOrSkip(t)
	open := runProbe(t, SandboxOff, probeSpec{Clone: true})
	expectOutcomes(t, "unconfined", open, "EINVAL", "clone3", "clone_newuser")
	shut := runProbe(t, SandboxAuto, probeSpec{Clone: true})
	expectOutcomes(t, "confined", shut, "ENOSYS", "clone3")
	expectOutcomes(t, "confined", shut, "EPERM", "clone_newuser")
}

// TestWorkerSandboxDeniesMetadataChanges: Landlock does not govern a
// file's mode, owner, times or extended attributes; the filter refuses
// changing them.
func TestWorkerSandboxDeniesMetadataChanges(t *testing.T) {
	seccompOrSkip(t)
	dir := t.TempDir()
	for _, run := range []struct {
		mode SandboxMode
		want string
	}{{SandboxOff, "ok"}, {SandboxAuto, "EPERM"}} {
		file := filepath.Join(dir, run.mode.String())
		if err := os.WriteFile(file, []byte("parent data"), 0o644); err != nil {
			t.Fatal(err)
		}
		expectOutcomes(t, "sandbox "+run.mode.String(), runProbe(t, run.mode, probeSpec{Chmod: file}), run.want, "chmod")
	}
}

// TestWorkerSandboxDeniesOtherProcesses: Landlock does not govern resource
// limits of another process of the same user; the filter refuses
// prlimit64 on any pid but the caller's own.
func TestWorkerSandboxDeniesOtherProcesses(t *testing.T) {
	seccompOrSkip(t)
	got := runProbe(t, SandboxAuto, probeSpec{Prlimit: true})
	expectOutcomes(t, "confined", got, "EPERM", "prlimit_parent")
	expectOutcomes(t, "confined", got, "ok", "prlimit_self")
}

// TestWorkerSandboxReprobesAfterFailure: a failed probe is not cached, a
// definite answer is.
func TestWorkerSandboxReprobesAfterFailure(t *testing.T) {
	probedSandbox(t)
	if supportOverride.Load() != nil {
		t.Fatal("a sandbox override is still set")
	}
	savedProbe, savedCached, savedLast := probeSupport, supportCached.Load(), supportLast.Load()
	t.Cleanup(func() {
		probeSupport = savedProbe
		supportCached.Store(savedCached)
		supportLast.Store(savedLast)
	})
	supportCached.Store(nil)
	supportLast.Store(nil)
	calls := 0
	probeSupport = func() sandboxSupport {
		calls++
		if calls == 1 {
			return sandboxSupport{reason: "the sandbox probe failed (signal: killed)"}
		}
		return sandboxSupport{abi: 7, seccomp: true, definite: true}
	}
	if s := sandboxSupported(); s.abi != 0 || calls != 1 {
		t.Fatalf("first probe: %+v after %d calls", s, calls)
	}
	if s := sandboxSupported(); s.abi != 7 || calls != 2 {
		t.Fatalf("second probe: %+v after %d calls", s, calls)
	}
	if s := sandboxSupported(); s.abi != 7 || calls != 2 {
		t.Fatalf("a definite answer was probed again: %+v after %d calls", s, calls)
	}
}

// runSeccomp evaluates filter for one call the way the kernel's classic
// BPF interpreter does, for the instructions the filter uses.
func runSeccomp(t *testing.T, filter []unix.SockFilter, arch uint32, nr uint32, arg0 uint64) uint32 {
	t.Helper()
	data := make([]byte, 64) // struct seccomp_data
	binary.LittleEndian.PutUint32(data[seccompDataNr:], nr)
	binary.LittleEndian.PutUint32(data[seccompDataArch:], arch)
	binary.LittleEndian.PutUint64(data[seccompDataArg0Low:], arg0)
	var a uint32
	for pc := 0; pc < len(filter); pc++ {
		ins := filter[pc]
		jump := func(cond bool) {
			if cond {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
		}
		switch ins.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			a = binary.LittleEndian.Uint32(data[ins.K:])
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			jump(a == ins.K)
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			jump(a >= ins.K)
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			jump(a&ins.K != 0)
		case unix.BPF_RET | unix.BPF_K:
			return ins.K
		default:
			t.Fatalf("instruction %d: unexpected code %#x", pc, ins.Code)
		}
	}
	t.Fatal("the filter ran past its end")
	return 0
}

// TestWorkerSandboxSeccompFilter checks the filter's verdicts without a
// kernel: the clone flags the runtime itself uses pass, namespace flags,
// clone3, the denied calls and foreign conventions do not.
func TestWorkerSandboxSeccompFilter(t *testing.T) {
	probedSandbox(t)
	const (
		allow   = uint32(unix.SECCOMP_RET_ALLOW)
		sigchld = uint64(unix.SIGCHLD)
	)
	f := workerSeccompFilter
	check := func(name string, arch, nr uint32, arg0 uint64, want uint32) {
		t.Helper()
		if got := runSeccomp(t, f, arch, nr, arg0); got != want {
			t.Errorf("%s: verdict %#x, want %#x", name, got, want)
		}
	}
	check("read", seccompAuditArch, unix.SYS_READ, 0, allow)
	check("os/exec fork", seccompAuditArch, unix.SYS_CLONE, unix.CLONE_VFORK|unix.CLONE_VM|unix.CLONE_PIDFD|sigchld, allow)
	check("runtime thread", seccompAuditArch, unix.SYS_CLONE,
		unix.CLONE_VM|unix.CLONE_FS|unix.CLONE_FILES|unix.CLONE_SIGHAND|unix.CLONE_SYSVSEM|unix.CLONE_THREAD, allow)
	check("clone3", seccompAuditArch, unix.SYS_CLONE3, 0, seccompNoSys)
	for name, flag := range map[string]uint64{
		"CLONE_NEWNS": unix.CLONE_NEWNS, "CLONE_NEWUTS": unix.CLONE_NEWUTS, "CLONE_NEWIPC": unix.CLONE_NEWIPC,
		"CLONE_NEWUSER": unix.CLONE_NEWUSER, "CLONE_NEWPID": unix.CLONE_NEWPID, "CLONE_NEWNET": unix.CLONE_NEWNET,
		"CLONE_NEWCGROUP": unix.CLONE_NEWCGROUP, "CLONE_PARENT": unix.CLONE_PARENT,
	} {
		check("clone "+name, seccompAuditArch, unix.SYS_CLONE, flag|sigchld, seccompDeny)
	}
	check("prlimit64 self", seccompAuditArch, unix.SYS_PRLIMIT64, 0, allow)
	check("prlimit64 other", seccompAuditArch, unix.SYS_PRLIMIT64, 4242, seccompDeny)
	check("prlimit64 other, high bits", seccompAuditArch, unix.SYS_PRLIMIT64, 1<<32|4242, seccompDeny)
	for _, nr := range append(append([]uint32(nil), deniedSyscalls...), seccompArchDenied...) {
		check(fmt.Sprintf("syscall %d", nr), seccompAuditArch, nr, 0, seccompDeny)
	}
	for name, nr := range map[string]uint32{"fchmodat": unix.SYS_FCHMODAT, "setpriority": unix.SYS_SETPRIORITY, "sched_setaffinity": unix.SYS_SCHED_SETAFFINITY} {
		check(name, seccompAuditArch, nr, 0, seccompDeny)
	}
	check("i386 read", unix.AUDIT_ARCH_I386, unix.SYS_READ, 0, seccompDeny)
	if runtime.GOARCH == "amd64" {
		check("x32 read", seccompAuditArch, unix.SYS_READ|0x40000000, 0, seccompDeny)
	}
}
