package codemode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// The parent side of a worker run: start the child with a bare
// environment, feed it the start message, answer its calls through the
// session, collect prints and the outcome, and turn any death of the child
// into a clean script error.
//
// Every wait on the child is bounded. A child whose stdout ends without an
// outcome is killed whatever it is doing (a message past the line limit
// leaves it blocked on a pipe nobody reads any more); every write to the
// child gives up at the script deadline; Wait has exec.Cmd.WaitDelay
// behind it. Nothing here holds the handler, the concurrency slot, the
// process or its pipes past the script's clock.

const (
	// stderrCap bounds how much of the worker's stderr is kept for
	// diagnostics.
	stderrCap = 64 << 10
	// exitGrace is how long a worker that reported its outcome gets to
	// exit on its own before it is killed.
	exitGrace = 2 * time.Second
	// waitDelay bounds Wait once the process is gone (pipes a stray
	// grandchild might still hold open).
	waitDelay = 3 * time.Second
)

// runInWorker takes a concurrency slot, or reports code mode busy, and
// executes start in a child process (runWorker).
func (r *Runtime) runInWorker(ctx context.Context, s *session, start startMsg, stopNote string) scriptResult {
	slot, err := r.AcquireSlot(ctx)
	switch {
	case errors.Is(err, ErrBusy):
		return scriptResult{errKind: "runtime", cause: ErrorBusy, errMsg: err.Error()}
	case err != nil:
		return scriptResult{errKind: "runtime", cause: ErrorTimeout,
			errMsg: fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote)}
	}
	defer slot.Release()
	return r.runWorker(ctx, s, start, stopNote)
}

// runWorker executes start in a child process; the caller holds a slot.
// The session performs the nested calls; the returned result carries the
// script's outcome or the reason the worker did not deliver one.
func (r *Runtime) runWorker(ctx context.Context, s *session, start startMsg, stopNote string) scriptResult {
	runtimeErr := func(msg string) scriptResult {
		return scriptResult{errKind: "runtime", errMsg: msg, cause: ErrorInternal}
	}
	stopped := func() scriptResult {
		return scriptResult{errKind: "runtime", cause: ErrorTimeout,
			errMsg: fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote)}
	}

	path, err := r.executablePath()
	if err != nil {
		return runtimeErr("could not start the script worker: " + err.Error())
	}
	mib := r.worker.MemoryMiB
	cmd := exec.CommandContext(ctx, path, r.worker.Args...)
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	cmd.WaitDelay = waitDelay
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
	if err := startConfined(cmd, path, r.worker.Sandbox); err != nil {
		return runtimeErr("could not start the script worker: " + err.Error())
	}
	r.lastPID.Store(int64(cmd.Process.Pid))
	// A confined worker cannot reach /proc to do this itself.
	preferOOMKillChild(cmd.Process.Pid)

	// The reader hands messages over until the child's stdout ends or a
	// message breaks the line limit; readErr says which.
	msgs := make(chan childMsg)
	readerDone := make(chan struct{})
	stop := make(chan struct{})
	var readErr error
	go func() {
		defer close(readerDone)
		br := bufio.NewReaderSize(stdout, 64<<10)
		for {
			line, err := readLine(br)
			if err != nil {
				readErr = err
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

	// finish tears the child down. Without an outcome the child is killed
	// at once; with one it gets exitGrace to leave on its own. Wait is
	// bounded by WaitDelay. Called once on every path out.
	var once sync.Once
	var waitErr error
	finish := func(gotOutcome bool) {
		once.Do(func() {
			close(stop)
			_ = stdin.Close()
			if !gotOutcome {
				killGroup(cmd)
			}
			select {
			case <-readerDone:
			case <-time.After(exitGrace):
				killGroup(cmd)
				select {
				case <-readerDone:
				case <-time.After(exitGrace):
				}
			}
			waitErr = cmd.Wait()
		})
	}
	defer finish(false)

	// write sends one message, giving up at the deadline: a child that has
	// stopped reading cannot hold the parent on a full pipe.
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b = append(b, '\n')
		errc := make(chan error, 1)
		go func() {
			_, err := stdin.Write(b)
			errc <- err
		}()
		select {
		case err := <-errc:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := write(start); err != nil {
		finish(false)
		if ctx.Err() != nil {
			return stopped()
		}
		return r.workerDied(ctx, stopNote, waitErr, readErr, stderr.String())
	}

	// Nested calls run beside this loop, at most maxParallelCalls at once;
	// calls past that wait, in order, for a free lane. Each is admitted
	// (numbered) here as its message arrives, so the numbers follow the
	// order the script asked in, and each answer is written back when its
	// call finishes, in whatever order they finish. The deadline watch
	// stays in charge: a tool that ignores its context cannot hold the
	// script past its clock, the worker is killed and the run returns on
	// time. answers has room for every call in flight, so a call that
	// finishes after the run returned never blocks.
	type answer struct {
		id  int
		msg parentMsg
	}
	answers := make(chan answer, maxParallelCalls)
	var queued []*preparedCall
	inflight := 0
	launch := func(pc *preparedCall) {
		inflight++
		go func() { answers <- answer{pc.req.id, s.perform(pc)} }()
	}
	reply := func(id int, msg parentMsg) error {
		msg.Type, msg.ID = msgResult, id
		return write(msg)
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			finish(false)
			return stopped()
		case m := <-msgs:
			switch m.Type {
			case msgPrint:
				s.print(m.Text)
			case msgCall:
				req := callRequest{id: m.ID, server: m.Server, member: m.Member, name: m.Name, args: m.Args, try: m.Try}
				pc, refusal := s.admit(req)
				switch {
				case refusal != nil:
					err = reply(m.ID, *refusal)
				case inflight < maxParallelCalls:
					launch(pc)
				default:
					queued = append(queued, pc)
				}
			case msgDone:
				finish(true)
				return scriptResult{result: m.Result, hasResult: m.HasResult}
			case msgError:
				finish(true)
				// The worker is not trusted with more than these two causes.
				cause := ""
				if m.Cause == ErrorTimeout || m.Cause == ErrorLimit {
					cause = m.Cause
				}
				return scriptResult{errKind: m.Kind, errMsg: m.Message, cause: cause, call: m.Call}
			}
		case a := <-answers:
			inflight--
			if len(queued) > 0 {
				launch(queued[0])
				queued = queued[1:]
			}
			err = reply(a.id, a.msg)
		case <-readerDone:
			// The child ended, or broke the protocol, without an outcome.
			finish(false)
			return r.workerDied(ctx, stopNote, waitErr, readErr, stderr.String())
		}
		if err != nil {
			finish(false)
			if ctx.Err() != nil {
				return stopped()
			}
			return r.workerDied(ctx, stopNote, waitErr, readErr, stderr.String())
		}
	}
}

// startConfined starts cmd, whose program is exe, confined as mode says.
// When it confines the child, a nil Stdin, Stdout or Stderr gets a
// /dev/null opened beforehand; the field then holds that *os.File, which
// is closed once the child has its copy.
func startConfined(cmd *exec.Cmd, exe string, mode SandboxMode) (err error) {
	defer func() {
		if err != nil {
			// A refusal before cmd.Start would leave the pipes cmd holds
			// (StdinPipe, StdoutPipe) open. Start with Err set returns it
			// without starting anything and closes them; after a Start
			// that already ran it only reports "already started".
			cmd.Err = err
			_ = cmd.Start()
		}
	}()
	switch mode {
	case SandboxOff:
		return cmd.Start()
	case SandboxAuto, SandboxRequired:
	default:
		return fmt.Errorf("unknown sandbox mode %d", int(mode))
	}
	support := sandboxSupported()
	logSandboxOnce(support)
	if mode == SandboxRequired {
		switch {
		case support.abi == 0:
			return fmt.Errorf("the worker sandbox is required, but Landlock is unavailable: %s", support.reason)
		case !support.seccomp:
			return fmt.Errorf("the worker sandbox is required, but no seccomp filter can be installed: %s", support.seccompReason)
		}
	}
	if support.abi == 0 && !support.seccomp {
		return cmd.Start()
	}
	if err := startSandboxed(cmd, exe, support); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	return nil
}

var sandboxLogOnce sync.Once

// logSandboxOnce records, once per process, what confinement workers get.
// A failed probe, which is retried, is logged each time.
func logSandboxOnce(s sandboxSupport) {
	if !s.definite {
		logx.For("codemode").Warn("code-mode sandbox probe failed; the next script probes again", "reason", s.reason)
		return
	}
	sandboxLogOnce.Do(func() {
		lg := logx.For("codemode")
		switch {
		case s.abi == 0 && !s.seccomp:
			lg.Warn("code-mode worker sandbox unavailable; scripts run with resource limits only (or are refused when the sandbox is required)",
				"reason", s.reason, "seccomp", s.seccompReason)
		case s.abi == 0:
			lg.Warn("code-mode worker sandbox: seccomp filter only, no Landlock (scripts are refused when the sandbox is required)",
				"reason", s.reason)
		case !s.seccomp:
			lg.Warn("code-mode worker sandbox: Landlock only, no seccomp filter", "landlock_abi", s.abi, "seccomp", s.seccompReason)
		default:
			lg.Info("code-mode worker sandbox", "landlock_abi", s.abi, "seccomp", true)
		}
	})
}

// workerDied explains a child that delivered no outcome.
func (r *Runtime) workerDied(ctx context.Context, stopNote string, waitErr, readErr error, stderr string) scriptResult {
	fail := func(msg string) scriptResult {
		return scriptResult{errKind: "runtime", errMsg: msg, cause: ErrorInternal}
	}
	if ctx.Err() != nil {
		return scriptResult{errKind: "runtime", cause: ErrorTimeout,
			errMsg: fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote)}
	}
	waitText := ""
	if waitErr != nil {
		waitText = waitErr.Error()
	}
	switch {
	case strings.Contains(stderr, "codemode-worker: memory cap") || strings.Contains(waitText, "bad system call"):
		detail := firstLine(stderr)
		if detail == "" {
			detail = waitText
		}
		return fail("code mode worker could not apply its memory cap (" + detail + "); " +
			"if toolyard runs under systemd with SystemCallFilter, allow setrlimit and prlimit64")
	case strings.Contains(stderr, "out of memory") || strings.Contains(stderr, "cannot allocate memory"):
		res := fail(fmt.Sprintf(
			"script exceeded the memory limit (%d MiB) and was stopped; work on less data at a time, or let the tool filter before returning",
			r.worker.MemoryMiB))
		res.cause = ErrorLimit
		return res
	case errors.Is(readErr, errLineTooLong):
		return fail("script worker crashed (it sent a message over the " + errLineTooLong.Error()[len("message exceeds the "):] + ")")
	}
	detail := "exited without a result"
	if waitText != "" {
		detail = waitText
	}
	if line := firstLine(stderr); line != "" {
		detail += ": " + line
	}
	return fail("script worker crashed (" + detail + ")")
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
