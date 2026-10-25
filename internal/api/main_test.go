package api

import (
	"os"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
)

// This is the package's only TestMain: add any other per-binary setup
// here rather than in a second one.
//
// Code-mode scripts run in a child process. In this package's tests the
// child is the test binary itself, which runs the code-mode worker instead
// of the tests when it sees apiTestWorkerEnv in the bare environment the
// parent builds (code_js_routes_test.go).
const apiTestWorkerEnv = "TOOLYARD_API_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(apiTestWorkerEnv) == "1" {
		os.Exit(codemode.WorkerMain(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
