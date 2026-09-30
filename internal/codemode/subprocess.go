package codemode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The parent side of a worker run: start the child with a bare
// environment, feed it the start message, answer its calls through the
// session, collect prints and the outcome, and turn any death of the child
// into a clean script error.

// stderrCap bounds how much of the worker's stderr is kept for diagnostics.
const stderrCap = 64 << 10

// runInWorker executes start in a child process. The session performs the
// nested calls; the returned result carries the script's outcome or the
// reason the worker did not deliver one.
func (r *Runtime) runInWorker(ctx context.Context, s *session, start startMsg, stopNote string) scriptResult {
	runtimeErr := func(msg string) scriptResult { return scriptResult{errKind: "runtime", errMsg: msg} }

	// A slot, or busy.
	select {
	case r.sem <- struct{}{}:
	case <-time.After(r.worker.AcquireWait):
		return runtimeErr(fmt.Sprintf("code mode is busy: %d scripts are already running; try again in a moment", r.worker.MaxConcurrent))
	case <-ctx.Done():
		return runtimeErr(fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote))
	}
	defer func() { <-r.sem }()

	path, err := r.executablePath()
	if err != nil {
		return runtimeErr("could not start the script worker: " + err.Error())
	}
	mib := r.worker.MemoryMiB
	cmd := exec.Command(path, r.worker.Args...)
	cmd.Env = append(append([]string(nil), r.worker.Env...),
		fmt.Sprintf("%s=%d", MemoryEnv, mib),
		fmt.Sprintf("GOMEMLIMIT=%dMiB", mib*3/4),
		"GOMAXPROCS=2",
		"GOTRACEBACK=none",
	)
	cmd.Dir = "/"
	ownProcessGroup(cmd)
	stderr := &cappedBuffer{max: stderrCap}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runtimeErr("could not start the script worker: " + err.Error())
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runtimeErr("could not start the script worker: " + err.Error())
	}
	if err := cmd.Start(); err != nil {
		return runtimeErr("could not start the script worker: " + err.Error())
	}
	r.lastPID.Store(int64(cmd.Process.Pid))

	// The reader hands messages over until the child's stdout closes.
	msgs := make(chan childMsg)
	readerDone := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(readerDone)
		br := bufio.NewReaderSize(stdout, 64<<10)
		for {
			line, err := readLine(br)
			if err != nil {
				return
			}
			var m childMsg
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			select {
			case msgs <- m:
			case <-stop:
				return
			}
		}
	}()

	// finish tears the child down: stop the reader, make sure the process
	// is gone, reap it. Called once on every path out.
	var once sync.Once
	var waitErr error
	finish := func(kill bool) {
		once.Do(func() {
			close(stop)
			_ = stdin.Close()
			if kill {
				killGroup(cmd)
			}
			select {
			case <-readerDone:
			case <-time.After(3 * time.Second):
				killGroup(cmd)
				<-readerDone
			}
			waitErr = cmd.Wait()
		})
	}
	defer finish(true)

	enc := json.NewEncoder(stdin)
	if err := enc.Encode(start); err != nil {
		finish(true)
		return runtimeErr("could not start the script worker: " + err.Error())
	}

	for {
		select {
		case <-ctx.Done():
			finish(true)
			return runtimeErr(fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote))
		case m, ok := <-msgs:
			if !ok {
				continue
			}
			switch m.Type {
			case msgPrint:
				s.print(m.Text)
			case msgCall:
				// The nested call runs beside the deadline watch: a tool
				// that ignores its context cannot hold the script past its
				// clock, the worker is killed and the run returns on time.
				replies := make(chan parentMsg, 1)
				go func(m childMsg) {
					replies <- s.handleCall(m.Server, m.Member, m.Args)
				}(m)
				var reply parentMsg
				select {
				case reply = <-replies:
				case <-ctx.Done():
					finish(true)
					return runtimeErr(fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote))
				}
				reply.Type, reply.ID = msgResult, m.ID
				if err := enc.Encode(reply); err != nil {
					finish(true)
					return r.workerDied(ctx, stopNote, waitErr, stderr.String())
				}
			case msgDone:
				finish(false)
				return scriptResult{result: m.Result, hasResult: m.HasResult}
			case msgError:
				finish(false)
				return scriptResult{errKind: m.Kind, errMsg: m.Message}
			}
		case <-readerDone:
			// The child ended without an outcome.
			finish(false)
			return r.workerDied(ctx, stopNote, waitErr, stderr.String())
		}
	}
}

// workerDied explains a child that exited without reporting an outcome.
func (r *Runtime) workerDied(ctx context.Context, stopNote string, waitErr error, stderr string) scriptResult {
	if ctx.Err() != nil {
		return scriptResult{errKind: "runtime", errMsg: fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote)}
	}
	if strings.Contains(stderr, "out of memory") || strings.Contains(stderr, "cannot allocate memory") {
		return scriptResult{errKind: "runtime", errMsg: fmt.Sprintf(
			"script exceeded the memory limit (%d MiB) and was stopped; work on less data at a time, or let the tool filter before returning",
			r.worker.MemoryMiB)}
	}
	detail := "exited without a result"
	if waitErr != nil {
		detail = waitErr.Error()
	}
	if line := firstLine(stderr); line != "" {
		detail += ": " + line
	}
	return scriptResult{errKind: "runtime", errMsg: "script worker crashed (" + detail + ")"}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// cappedBuffer keeps the first max bytes written and drops the rest.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}
