package codemode

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// The worker tests spawn this test binary as the worker: TestMain, seeing
// the marker variable in the bare environment the parent builds, runs
// WorkerMain instead of the tests.
const testWorkerEnv = "TOOLYARD_CODEMODE_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(testWorkerEnv) == "1" {
		os.Exit(WorkerMain(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// testWorker runs scripts in a child that is this test binary.
func testWorker() Worker {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return Worker{Path: exe, Env: []string{testWorkerEnv + "=1"}, MemoryMiB: 256, AcquireWait: 200 * time.Millisecond}
}

func workerRuntime(f Caller, l Limits, adjust func(*Worker)) *Runtime {
	rt := New(f, l)
	w := testWorker()
	if adjust != nil {
		adjust(&w)
	}
	rt.SetWorker(w)
	return rt
}

// processGone reports whether pid no longer exists.
func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

func TestWorkerRunsPlainScript(t *testing.T) {
	rt := workerRuntime(newFake(), Limits{}, nil)
	text, failed := rt.ExecuteToolCode(context.Background(), "print(\"hi\")\nresult = {\"k\": [1, 2.5, \"s\", None, True]}", "")
	if failed || !strings.HasPrefix(text, "Print output:\nhi\n\nExecution completed successfully.\nReturn value: {\n  \"k\": [\n    1,\n    2.5,\n    \"s\",\n    null,\n    true\n  ]\n}") {
		t.Fatalf("worker output (failed=%v):\n%s", failed, text)
	}
	if !processGone(int(rt.lastPID.Load())) {
		t.Fatalf("worker %d is still alive", rt.lastPID.Load())
	}
	// Errors travel with their kind.
	text, failed = rt.ExecuteToolCode(context.Background(), "def broken(:\n  pass", "")
	if !failed || !strings.HasPrefix(text, "Execution syntax error:\n\ncode.star:1:13: got ':', want ')'") {
		t.Fatalf("worker syntax error:\n%s", text)
	}
	text, failed = rt.ExecuteToolCode(context.Background(), "x = {}\nresult = x[\"nope\"]", "")
	if !failed || !strings.Contains(text, "Error: key \"nope\" not in dict") || !strings.Contains(text, "Dictionary key not found.") {
		t.Fatalf("worker runtime error:\n%s", text)
	}
}

func TestWorkerHugeAllocationIsCleanError(t *testing.T) {
	f := newFake()
	rt := workerRuntime(f, Limits{}, nil)
	code := "print(\"about to allocate\")\nbig = [0] * (1 << 28)\nresult = len(big)"
	text, failed := rt.ExecuteToolCode(context.Background(), code, "")
	if !failed || !strings.Contains(text, "Execution runtime error:\n\nscript exceeded the memory limit (256 MiB)") {
		t.Fatalf("huge allocation (failed=%v):\n%s", failed, text)
	}
	if !strings.Contains(text, "Print Output:\nabout to allocate") {
		t.Fatalf("prints before the death should survive:\n%s", text)
	}
	if !processGone(int(rt.lastPID.Load())) {
		t.Fatalf("worker %d is still alive", rt.lastPID.Load())
	}
	// The parent is fine: the next script runs, in a fresh worker.
	text, failed = rt.ExecuteToolCode(context.Background(), multiCallScript, "outer reason from the client, long enough")
	if failed || text != multiCallWant {
		t.Fatalf("after the crash (failed=%v):\n%s", failed, text)
	}
	// Growing in a loop dies the same way.
	text, failed = rt.ExecuteToolCode(context.Background(), "l = []\nfor i in range(1 << 30):\n  l.append(\"x\" * 1024)\nresult = len(l)", "")
	if !failed || !strings.Contains(text, "script exceeded the memory limit") {
		t.Fatalf("slow growth (failed=%v):\n%s", failed, text)
	}
}

func TestWorkerTimeoutKillsProcess(t *testing.T) {
	slow := newFake()
	release := make(chan struct{})
	slow.handlers["BkCoreServices.get-all-clients"] = func(map[string]any) (*mcp.CallToolResult, error) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return jsonResult([]any{})
	}
	rt := workerRuntime(slow, Limits{ScriptTimeout: 300 * time.Millisecond}, nil)
	start := time.Now()
	text, failed := rt.ExecuteToolCode(context.Background(), "result = BkCoreServices.get_all_clients()", "")
	close(release)
	if !failed || !strings.Contains(text, "script stopped: context deadline exceeded (limit 300ms)") {
		t.Fatalf("timeout (failed=%v):\n%s", failed, text)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("timeout took %s", took)
	}
	if !processGone(int(rt.lastPID.Load())) {
		t.Fatalf("worker %d survived the timeout", rt.lastPID.Load())
	}

	// A spin with no calls is stopped inside the worker by its own clock.
	rt = workerRuntime(newFake(), Limits{ScriptTimeout: 300 * time.Millisecond, MaxSteps: 1 << 62}, nil)
	text, failed = rt.ExecuteToolCode(context.Background(), "i = 0\nwhile True:\n  i += 1\nresult = i", "")
	if !failed || !strings.Contains(text, "script stopped: context deadline exceeded (limit 300ms)") {
		t.Fatalf("spin (failed=%v):\n%s", failed, text)
	}
	if !processGone(int(rt.lastPID.Load())) {
		t.Fatalf("spinning worker %d survived", rt.lastPID.Load())
	}
}

func TestWorkerConcurrencyIsCapped(t *testing.T) {
	slow := newFake()
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	slow.handlers["BkCoreServices.get-all-clients"] = func(map[string]any) (*mcp.CallToolResult, error) {
		started <- struct{}{}
		<-release
		return jsonResult([]any{})
	}
	rt := workerRuntime(slow, Limits{}, func(w *Worker) { w.MaxConcurrent = 2; w.AcquireWait = 100 * time.Millisecond })

	var wg sync.WaitGroup
	var mu sync.Mutex
	var outputs []string
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, _ := rt.ExecuteToolCode(context.Background(), "result = BkCoreServices.get_all_clients()", "")
			mu.Lock()
			outputs = append(outputs, text)
			mu.Unlock()
		}()
	}
	<-started
	<-started
	// Both slots are taken and blocked in a nested call; a third run waits
	// briefly, then is refused without starting a worker.
	text, failed := rt.ExecuteToolCode(context.Background(), "result = 1", "")
	if !failed || !strings.Contains(text, "code mode is busy: 2 scripts are already running; try again in a moment") {
		t.Fatalf("third run (failed=%v):\n%s", failed, text)
	}
	close(release)
	wg.Wait()
	for _, out := range outputs {
		if !strings.Contains(out, "Execution completed successfully") {
			t.Fatalf("a capped run failed:\n%s", out)
		}
	}
	// Slots are free again.
	if text, failed := rt.ExecuteToolCode(context.Background(), "result = 1", ""); failed {
		t.Fatalf("after release:\n%s", text)
	}
}

func TestWorkerCrashIsCleanError(t *testing.T) {
	// A worker that exits without reporting.
	rt := workerRuntime(newFake(), Limits{}, func(w *Worker) { w.Path = "/bin/sh"; w.Args = []string{"-c", "echo boom >&2; exit 3"}; w.Env = nil })
	text, failed := rt.ExecuteToolCode(context.Background(), "result = 1", "")
	if !failed || !strings.Contains(text, "Execution runtime error:\n\nscript worker crashed (exit status 3: boom)") {
		t.Fatalf("crash (failed=%v):\n%s", failed, text)
	}
	// A worker that cannot start.
	rt = workerRuntime(newFake(), Limits{}, func(w *Worker) { w.Path = "/nonexistent/toolyard" })
	text, failed = rt.ExecuteToolCode(context.Background(), "result = 1", "")
	if !failed || !strings.Contains(text, "could not start the script worker") {
		t.Fatalf("no binary (failed=%v):\n%s", failed, text)
	}
	// A worker that reports garbage and hangs is killed by the wall clock.
	rt = workerRuntime(newFake(), Limits{ScriptTimeout: 300 * time.Millisecond}, func(w *Worker) {
		w.Path = "/bin/sh"
		w.Args = []string{"-c", "echo not-json; sleep 30"}
		w.Env = nil
	})
	text, failed = rt.ExecuteToolCode(context.Background(), "result = 1", "")
	if !failed || !strings.Contains(text, "script stopped: context deadline exceeded (limit 300ms)") {
		t.Fatalf("hang (failed=%v):\n%s", failed, text)
	}
	if !processGone(int(rt.lastPID.Load())) {
		t.Fatalf("hung worker %d survived", rt.lastPID.Load())
	}
}

func TestWorkerEnvironmentIsBare(t *testing.T) {
	t.Setenv("TOOLYARD_SECRET_FOR_TEST", "must-not-leak")
	rt := workerRuntime(newFake(), Limits{}, func(w *Worker) {
		w.Path = "/bin/sh"
		// Report the environment as a fake "done" message.
		w.Args = []string{"-c", `printf '{"type":"done","hasResult":true,"result":"%s"}\n' "$(env | tr '\n' ' ' | tr -d '"')"`}
		w.Env = nil
	})
	text, failed := rt.ExecuteToolCode(context.Background(), "result = 1", "")
	if failed {
		t.Fatalf("env probe:\n%s", text)
	}
	if strings.Contains(text, "must-not-leak") || strings.Contains(text, "PATH=") || strings.Contains(text, "HOME=") {
		t.Fatalf("worker environment leaks the parent's:\n%s", text)
	}
	for _, want := range []string{MemoryEnv + "=256", "GOMEMLIMIT=192MiB", "GOMAXPROCS=2", "GOTRACEBACK=none"} {
		if !strings.Contains(text, want) {
			t.Fatalf("worker environment missing %s:\n%s", want, text)
		}
	}
}

func TestLimitsText(t *testing.T) {
	rt := New(newFake(), Limits{})
	if got := rt.LimitsText(); got != "Limits per run: 5 minutes wall clock, 100 tool calls, 1 MiB of output, 512 MiB of memory." {
		t.Fatalf("LimitsText = %q", got)
	}
	rt = New(newFake(), Limits{ScriptTimeout: 90 * time.Second, MaxCalls: 7, MaxOutputBytes: 3 << 20})
	rt.SetWorker(Worker{MemoryMiB: 128})
	if got := rt.LimitsText(); got != "Limits per run: 1m30s wall clock, 7 tool calls, 3 MiB of output, 128 MiB of memory." {
		t.Fatalf("LimitsText = %q", got)
	}
}
