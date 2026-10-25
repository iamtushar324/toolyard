package codemode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// JavaScript scripts run in QuickJS-NG, compiled to WebAssembly by its
// own release pipeline (quickjs/qjs-wasi.wasm, the official qjs-wasi.wasm
// asset of the release named in QuickJSVersion, embedded unchanged), and
// run by wazero, a WebAssembly runtime in pure Go. Inside the worker the
// script is therefore boxed twice: by WebAssembly (the module can only
// touch its own linear memory and the WASI calls wazero gives it: no
// preopened directories, no environment, no sockets, stdin and stdout
// wired to the host below), and by the worker's process sandbox.
//
// wazero runs the module in its interpreter. Its compiler would need
// executable memory, which the sandbox allows (mprotect is not filtered),
// but it compiles the 1.5 MB module anew in every worker, and every
// script gets a fresh worker; interpreting starts in a fraction of that
// and is fast enough for scripts that mostly wait on tools.
//
// The limits are the worker's: the wall clock (wazero ends the module when
// the run's context ends, checked in loops and calls), the memory cap, the
// output caps and the call limit (the parent's). The memory cap
// (RLIMIT_DATA) covers the Go heap and the WebAssembly memory alike, and
// the WebAssembly memory is not Go heap (jsmem_unix.go), so the worker
// splits it (jsBudget): QuickJS's own heap limit, below the WebAssembly
// memory limit so running out is a catchable "out of memory" error; and
// a Go memory limit for the rest, which keeps the Go heap (wazero's
// interpreted module, the answers on their way in) from taking the
// WebAssembly memory's share.
//
// js_prelude.js, run by qjs with -e, does the rest in JavaScript: the
// tools object, console, the call pump, and reporting the outcome. This
// file is the other end of its stdin and stdout.

// QuickJSVersion names the embedded engine.
const QuickJSVersion = "QuickJS-NG v0.17.0"

// quickJSWasmSHA256 is the sha256 of quickjs/qjs-wasi.wasm, as GitHub
// publishes it for the release asset; TestEmbeddedQuickJS checks it.
const quickJSWasmSHA256 = "42a732a676ec2d93488c19411e0fad283bf72658fdad746f089914b523c783b1"

//go:embed quickjs/qjs-wasi.wasm
var quickJSWasm []byte

//go:embed js_prelude.js
var jsPrelude string

const (
	// jsHeadroomMiB is WebAssembly memory past QuickJS's heap limit: the
	// module's data and 2 MiB C stack, and the allocator's slack.
	jsHeadroomMiB = 24
	// jsSlackMiB is the part of the worker's cap neither the Go limit nor
	// the WebAssembly memory claims: the runtime's own mappings.
	jsSlackMiB = 8
	// jsMinHeapMiB is the smallest heap a script gets.
	jsMinHeapMiB = 16
	// jsStackSize is QuickJS's stack limit. Two ceilings sit above it: the
	// module's 2 MiB C stack, and wazero's interpreter, which allows 2,000
	// WebAssembly call frames (one or more per JavaScript call). 192 KiB
	// lets QuickJS raise its own RangeError ("Maximum call stack size
	// exceeded") well before either, a little over a thousand calls deep for
	// the smallest functions; anything that still reaches wazero's ceiling
	// is reported as the same RangeError.
	jsStackSize = "192k"
	// jsStderrCap bounds what is kept of the engine's stderr.
	jsStderrCap = 16 << 10
)

// jsBoot is the first line the prelude reads.
type jsBoot struct {
	Code        string       `json:"code"`
	Servers     []wireServer `json:"servers"`
	MaxParallel int          `json:"maxParallel"`
}

// jsMsg is a line the prelude writes.
type jsMsg struct {
	T string `json:"t"`
	// call
	ID  int             `json:"id"`
	S   string          `json:"s"`
	M   string          `json:"m"`
	N   string          `json:"n"`
	A   json.RawMessage `json:"a"`
	Try bool            `json:"try"`
	// print
	Text string `json:"text"`
	// done
	Has    bool            `json:"has"`
	Result json.RawMessage `json:"result"`
	// error
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Cause   string `json:"cause"`
	Call    int    `json:"call"`
}

// jsReply answers one call.
type jsReply struct {
	ID         int             `json:"id"`
	OK         bool            `json:"ok"`
	Text       string          `json:"text,omitempty"`
	JSON       json.RawMessage `json:"json,omitempty"`
	Status     string          `json:"status,omitempty"`
	Error      string          `json:"error,omitempty"`
	ApprovalID string          `json:"approvalId,omitempty"`
}

// jsBudget splits a worker's memory cap of mib MiB: QuickJS's heap limit,
// the WebAssembly memory limit (the heap and jsHeadroomMiB), and the Go
// memory limit (the rest, less jsSlackMiB). With the 256 MiB default:
// 88, 112 and 136 MiB.
func jsBudget(mib int) (heapMiB, wasmMiB, goMiB int) {
	heapMiB = max(mib*11/32, jsMinHeapMiB)
	wasmMiB = heapMiB + jsHeadroomMiB
	goMiB = max(mib-wasmMiB-jsSlackMiB, 32)
	return heapMiB, wasmMiB, goMiB
}

var (
	quickJSCheckOnce sync.Once
	quickJSCheckErr  error
)

// checkQuickJS verifies, once per process, that the embedded engine is
// the build quickJSWasmSHA256 names: a damaged or swapped binary is
// refused rather than run.
func checkQuickJS() error {
	quickJSCheckOnce.Do(func() {
		sum := sha256.Sum256(quickJSWasm)
		if got := hex.EncodeToString(sum[:]); got != quickJSWasmSHA256 {
			quickJSCheckErr = fmt.Errorf("the embedded JavaScript engine has sha256 %s, not %s's %s", got, QuickJSVersion, quickJSWasmSHA256)
		}
	})
	return quickJSCheckErr
}

// sleepUntil is the module's nanosleep: it ends early when ctx ends, so a
// script blocked in a sleep (qjs:os's os.sleep, say) stops at its
// deadline, in process as in a worker.
func sleepUntil(ctx context.Context) func(ns int64) {
	return func(ns int64) {
		if ns <= 0 {
			return
		}
		t := time.NewTimer(time.Duration(ns))
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}

// runJS executes JavaScript code under the limits; stopNote names the
// deadline reported when ctx ends the run.
func runJS(ctx context.Context, start startMsg, s sink, stopNote string) scriptResult {
	internal := func(msg string) scriptResult {
		return scriptResult{errKind: "runtime", cause: ErrorInternal, errMsg: msg}
	}
	if err := checkQuickJS(); err != nil {
		return internal(err.Error())
	}
	mib := start.MemoryMiB
	if mib <= 0 {
		mib = 256
	}
	heapMiB, wasmMiB, _ := jsBudget(mib)
	maxParallel := start.MaxParallel
	if maxParallel <= 0 || maxParallel > maxParallelCalls {
		maxParallel = maxParallelCalls
	}

	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h := &jsHost{ctx: hctx, s: s, ids: map[int]int{}}
	h.in = &jsStdin{ctx: hctx, lines: make(chan []byte, 2*maxParallelCalls)}
	h.out = &jsStdout{h: h}
	boot, err := json.Marshal(jsBoot{Code: start.Code, Servers: start.Servers, MaxParallel: maxParallel})
	if err != nil {
		return internal("could not start the JavaScript engine: " + err.Error())
	}
	h.in.lines <- append(boot, '\n')

	rctx := withJSMemory(ctx)
	cfg := wazero.NewRuntimeConfigInterpreter().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(uint32(wasmMiB * 16)) // 64 KiB pages
	r := wazero.NewRuntimeWithConfig(rctx, cfg)
	defer r.Close(context.Background())
	if _, err := wasi_snapshot_preview1.Instantiate(rctx, r); err != nil {
		return internal("could not start the JavaScript engine: " + err.Error())
	}
	compiled, err := r.CompileModule(rctx, quickJSWasm)
	if err != nil {
		return internal("could not load the JavaScript engine: " + err.Error())
	}
	stderr := &cappedBuffer{max: jsStderrCap}
	mc := wazero.NewModuleConfig().
		WithName("qjs").
		WithArgs("qjs", "--std", "--memory-limit", fmt.Sprintf("%dm", heapMiB), "--stack-size", jsStackSize, "-e", jsPrelude).
		WithStdin(h.in).
		WithStdout(h.out).
		WithStderr(stderr).
		WithSysWalltime().
		WithSysNanotime().
		WithNanosleep(sleepUntil(ctx)).
		WithRandSource(rand.Reader)
	_, runErr := r.InstantiateModule(rctx, compiled, mc)
	return h.outcome(ctx, runErr, stderr.String(), stopNote, heapMiB)
}

// jsHost is the Go end of the prelude's stdin and stdout.
type jsHost struct {
	ctx context.Context
	s   sink
	in  *jsStdin
	out *jsStdout

	mu sync.Mutex
	// final is the prelude's done or error line.
	final *jsMsg
	// ids maps the prelude's call ids to the sink's.
	ids map[int]int
	// protoErr is the first protocol violation (an overlong line).
	protoErr string
}

// handle acts on one line from the prelude. A line that is not one of its
// messages is ignored.
func (h *jsHost) handle(line []byte) {
	var m jsMsg
	if json.Unmarshal(line, &m) != nil {
		return
	}
	switch m.T {
	case "print":
		h.s.print(m.Text)
	case "call":
		h.call(m)
	case "done", "error":
		h.mu.Lock()
		if h.final == nil {
			h.final = &m
		}
		h.mu.Unlock()
	}
}

// call hands one call to the sink and answers the prelude when it
// finishes. Answers go out from goroutines: the prelude reads them on the
// engine's own goroutine, which is the one calling handle.
func (h *jsHost) call(m jsMsg) {
	var args map[string]any
	switch {
	case len(m.A) > maxArgsBytes:
		go h.reply(m.ID, parentMsg{Status: StatusFailed, Error: fmt.Sprintf("arguments to %s are %d bytes; one call may pass at most %d MiB. Pass less, or split the work across calls",
			callRequest{server: m.S, member: m.M, name: m.N}.label(), len(m.A), maxArgsBytes>>20)})
		return
	case len(m.A) > 0 && string(m.A) != "null":
		if err := json.Unmarshal(m.A, &args); err != nil {
			go h.reply(m.ID, parentMsg{Status: StatusFailed, Error: "tool arguments must be a JSON object: " + err.Error()})
			return
		}
	}
	id, ch := h.s.start(callRequest{server: m.S, member: m.M, name: m.N, args: args, try: m.Try})
	h.mu.Lock()
	h.ids[m.ID] = id
	h.mu.Unlock()
	go func(jsID int) {
		select {
		case r := <-ch:
			h.reply(jsID, r)
		case <-h.ctx.Done():
		}
	}(m.ID)
}

// reply queues one answer for the prelude. A successful answer whose text
// is JSON goes as that JSON value, so the engine parses it once, with the
// rest of the line, instead of receiving it as a string to parse again.
func (h *jsHost) reply(jsID int, r parentMsg) {
	rep := jsReply{ID: jsID, OK: r.Error == "", Status: r.Status, Error: r.Error, ApprovalID: r.ApprovalID}
	var value []byte
	switch {
	case len(r.JSON) > 0:
		value = r.JSON
	case r.Error == "" && r.Text != "":
		var buf bytes.Buffer
		buf.Grow(len(r.Text) + 64)
		if json.Compact(&buf, []byte(r.Text)) == nil {
			value = buf.Bytes()
		} else {
			rep.Text = r.Text
		}
	default:
		rep.Text = r.Text
	}
	if value != nil {
		// The value is valid, compact JSON already: splice it in rather
		// than have json.Marshal check and copy it again (it can be 6 MiB,
		// and the worker's Go heap is what the WebAssembly memory shares
		// its cap with).
		head, err := json.Marshal(rep)
		if err == nil {
			line := make([]byte, 0, len(head)+len(value)+16)
			line = append(line, head[:len(head)-1]...)
			line = append(line, `,"json":`...)
			line = append(line, value...)
			line = append(line, "}\n"...)
			select {
			case h.in.lines <- line:
			case <-h.ctx.Done():
			}
			return
		}
	}
	b, err := json.Marshal(rep)
	if err != nil {
		b, _ = json.Marshal(jsReply{ID: jsID, Status: StatusFailed, Error: "the tool's answer could not be passed to the script: " + err.Error()})
	}
	select {
	case h.in.lines <- append(b, '\n'):
	case <-h.ctx.Done():
	}
}

func (h *jsHost) protocolError(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.protoErr == "" {
		h.protoErr = msg
	}
}

// outcome reads how the engine ended: the prelude's report when it made
// one, else why it stopped.
func (h *jsHost) outcome(ctx context.Context, runErr error, stderr, stopNote string, heapMiB int) scriptResult {
	h.mu.Lock()
	final, proto := h.final, h.protoErr
	h.mu.Unlock()
	if final != nil && final.T == "done" {
		raw := bytes.TrimSpace(final.Result)
		if !final.Has || len(raw) == 0 || string(raw) == "null" {
			return scriptResult{}
		}
		if len(raw) > maxResultBytes {
			return resultTooBig(len(raw))
		}
		if !json.Valid(raw) {
			return scriptResult{errKind: "runtime", cause: ErrorInternal, errMsg: "the script returned a value that is not valid JSON"}
		}
		return scriptResult{result: json.RawMessage(raw), hasResult: true}
	}
	if final != nil {
		kind := "runtime"
		if final.Kind == "syntax" {
			kind = "syntax"
		}
		msg := cut(final.Message, maxErrorBytes, "… [error message truncated at 64 KiB]")
		if msg == "" {
			msg = "the script failed without a message"
		}
		res := scriptResult{errKind: kind, errMsg: msg}
		if final.Call != 0 {
			h.mu.Lock()
			res.call = h.ids[final.Call]
			h.mu.Unlock()
		}
		switch {
		case ctx.Err() != nil:
			res.cause = ErrorTimeout
		case final.Cause == "limit" || strings.Contains(msg, "out of memory") || strings.Contains(msg, "ran out of memory"):
			res.cause = ErrorLimit
		}
		return res
	}
	if ctx.Err() != nil {
		return scriptResult{errKind: "runtime", cause: ErrorTimeout,
			errMsg: fmt.Sprintf("script stopped: %v (%s)", ctx.Err(), stopNote)}
	}
	if proto != "" {
		return scriptResult{errKind: "runtime", errMsg: proto}
	}
	detail := strings.TrimSpace(stderr)
	var exit *sys.ExitError
	switch {
	case strings.Contains(detail, "out of memory"):
		return scriptResult{errKind: "runtime", cause: ErrorLimit, errMsg: fmt.Sprintf(
			"script exceeded the JavaScript memory limit (%d MiB) and was stopped; work on less data at a time, or let the tool filter before returning", heapMiB)}
	case detail != "":
		return scriptResult{errKind: "runtime", errMsg: cut(detail, maxErrorBytes, "… [error message truncated at 64 KiB]")}
	case errors.As(runErr, &exit) && exit.ExitCode() != 0:
		return scriptResult{errKind: "runtime", errMsg: fmt.Sprintf("the JavaScript engine exited with code %d before the script finished", exit.ExitCode())}
	case runErr != nil && strings.Contains(runErr.Error(), "stack overflow"):
		return scriptResult{errKind: "runtime", errMsg: "RangeError: Maximum call stack size exceeded (the script recursed deeper than the engine allows)"}
	case runErr != nil:
		return scriptResult{errKind: "runtime", errMsg: "the JavaScript engine stopped: " + firstLine(runErr.Error())}
	}
	return scriptResult{errKind: "runtime", errMsg: "the script ended without finishing: its top-level promise never settled, because it awaited a promise nothing resolves (tool calls always settle; code mode has no global setTimeout)"}
}

// jsStdin is the prelude's stdin: the boot line, then the answers.
// Reads come from the engine's goroutine only.
type jsStdin struct {
	ctx   context.Context
	lines chan []byte
	buf   []byte
}

func (r *jsStdin) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		select {
		case line := <-r.lines:
			r.buf = line
		case <-r.ctx.Done():
			return 0, io.EOF
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// jsStdout is the prelude's stdout, cut into lines. Writes come from the
// engine's goroutine only.
type jsStdout struct {
	h    *jsHost
	buf  []byte
	over bool // discarding the rest of an overlong line
}

func (w *jsStdout) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !w.over {
				w.buf = append(w.buf, p...)
				if len(w.buf) > maxLineBytes {
					w.over, w.buf = true, nil
					w.h.protocolError(fmt.Sprintf("the script wrote a message over the %d MiB line limit to the engine's output", maxLineBytes>>20))
				}
			}
			break
		}
		if w.over {
			w.over = false
		} else {
			line := append(w.buf, p[:i]...)
			w.buf = nil
			w.h.handle(line)
		}
		p = p[i+1:]
	}
	return n, nil
}
