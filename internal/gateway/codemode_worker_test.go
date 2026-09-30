package gateway

import (
	"os"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
)

// Code-mode scripts run in a child process. In tests that child is this
// test binary: TestMain, seeing the marker variable in the bare environment
// the parent builds, runs the worker instead of the tests, and every
// gateway built by a test points its runtime at that worker.
const testWorkerEnv = "TOOLYARD_GATEWAY_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(testWorkerEnv) == "1" {
		os.Exit(codemode.WorkerMain(os.Stdin, os.Stdout, os.Stderr))
	}
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	defaultCodeModeWorker = func() codemode.Worker {
		return codemode.Worker{Path: exe, Env: []string{testWorkerEnv + "=1"}}
	}
	os.Exit(m.Run())
}
