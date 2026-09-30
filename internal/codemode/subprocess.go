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

// runInWorker executes start in a child process. The session performs the
// nested calls; the returned result carries the script's outcome or the
// reason the worker did not deliver one.
func (r *Runtime) runInWorker(ctx context.Context, s *session, start startMsg, stopNote string) scriptResult {
	runtimeErr := func(msg string) scriptResult { return scriptResult{errKind: "runtime", errMsg: msg} }
	stopped := func() scriptResult {
		return runtimeErr(fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote))
	}

	// A slot, or busy.
	select {
	case r.sem <- struct{}{}:
	case <-time.After(r.worker.AcquireWait):
		return runtimeErr(fmt.Sprintf("code mode is busy: %d scripts are already running; try again in a moment", r.worker.MaxConcurrent))
	case <-ctx.Done():
		return stopped()
	}
	defer func() { <-r.sem }()

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
	if err := cmd.Start(); err != nil {
		return runtimeErr("could not start the script worker: " + err.Error())
	}
	r.lastPID.Store(int64(cmd.Process.Pid))

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

	for {
		select {
		case <-ctx.Done():
			finish(false)
			return stopped()
		case m := <-msgs:
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
					finish(false)
					return stopped()
				}
				reply.Type, reply.ID = msgResult, m.ID
				if err := write(reply); err != nil {
					finish(false)
					if ctx.Err() != nil {
						return stopped()
					}
					return r.workerDied(ctx, stopNote, waitErr, readErr, stderr.String())
				}
			case msgDone:
				finish(true)
				return scriptResult{result: m.Result, hasResult: m.HasResult}
			case msgError:
				finish(true)
				return scriptResult{errKind: m.Kind, errMsg: m.Message}
			}
		case <-readerDone:
			// The child ended, or broke the protocol, without an outcome.
			finish(false)
			return r.workerDied(ctx, stopNote, waitErr, readErr, stderr.String())
		}
	}
}

// workerDied explains a child that delivered no outcome.
func (r *Runtime) workerDied(ctx context.Context, stopNote string, waitErr, readErr error, stderr string) scriptResult {
	fail := func(msg string) scriptResult { return scriptResult{errKind: "runtime", errMsg: msg} }
	if ctx.Err() != nil {
		return fail(fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote))
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
		return fail(fmt.Sprintf(
			"script exceeded the memory limit (%d MiB) and was stopped; work on less data at a time, or let the tool filter before returning",
			r.worker.MemoryMiB))
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
