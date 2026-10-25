package codemode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
)

// The worker process. WorkerMain is the whole life of the child the
// gateway starts for one executeToolCode run: cap memory, read the start
// message, run the script (Starlark, or JavaScript in QuickJS), report,
// exit.

// MemoryEnv carries the memory cap, in MiB, from the parent to the worker.
// It is the one setting the worker needs before it reads anything.
const MemoryEnv = "TOOLYARD_CODEMODE_MEMORY_MIB"

// WorkerMain runs one script as the gateway's child process: stdin carries
// the start message and call replies, stdout the calls, prints and the
// outcome. The memory cap is applied before anything is read. The exit
// code is 0 once the outcome was reported, 1 on a setup or protocol
// failure (the parent reports either as a worker failure).
func WorkerMain(stdin io.Reader, stdout, stderr io.Writer) int {
	ignoreStopSignals()
	mib := 512
	if v, err := strconv.Atoi(os.Getenv(MemoryEnv)); err == nil && v > 0 {
		mib = v
	}
	if err := applyMemoryCap(mib); err != nil {
		fmt.Fprintln(stderr, "codemode-worker: memory cap:", err)
		return 1
	}
	debug.SetMemoryLimit(int64(mib) << 20 * 3 / 4)
	preferOOMKill()

	r := bufio.NewReaderSize(stdin, 64<<10)
	line, err := readLine(r)
	if err != nil {
		fmt.Fprintln(stderr, "codemode-worker: read start:", err)
		return 1
	}
	var start startMsg
	if err := json.Unmarshal(line, &start); err != nil || start.Type != msgStart {
		fmt.Fprintln(stderr, "codemode-worker: bad start message")
		return 1
	}
	applyCPUCap(start.Limits.ScriptTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), start.Limits.ScriptTimeout)
	defer cancel()
	if start.MemoryMiB <= 0 {
		start.MemoryMiB = mib
	}
	p := &pipeSink{r: r, w: stdout, limits: start.Limits, waiting: map[int]chan parentMsg{}}
	stopNote := fmt.Sprintf("limit %s", start.Limits.ScriptTimeout)
	var res scriptResult
	if start.Language == LanguageJavaScript {
		res = runJS(ctx, start, p, stopNote)
	} else {
		res = runScript(ctx, start, p, stopNote)
	}
	if err := p.finish(res); err != nil {
		fmt.Fprintln(stderr, "codemode-worker: report:", err)
		return 1
	}
	return 0
}

// pipeSink is the parent at the other end of stdin and stdout. Calls go
// out as they are made; one reader takes every answer off stdin and hands
// it to the call it names, so a script may have several calls in flight.
type pipeSink struct {
	r      *bufio.Reader
	w      io.Writer
	limits Limits

	// wmu serialises writes. sent counts print bytes; past the limit one
	// crossing print goes out (the parent turns it into the truncation
	// note) and the rest stay here.
	wmu    sync.Mutex
	sent   int
	capped bool

	mu      sync.Mutex
	nextID  int
	waiting map[int]chan parentMsg
	// readErr is set once the reader stopped: the gateway went away.
	readErr    error
	readerOnce sync.Once
}

func (p *pipeSink) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.w.Write(append(b, '\n'))
	return err
}

func (p *pipeSink) print(text string) {
	p.wmu.Lock()
	if p.capped {
		p.wmu.Unlock()
		return
	}
	if len(text) > maxPrintBytes {
		text = text[:maxPrintBytes] + "… [print truncated]"
	}
	if p.sent+len(text) > p.limits.MaxOutputBytes {
		p.capped = true
		if len(text) > 1024 {
			text = text[:1024]
		}
	}
	p.sent += len(text) + 1
	p.wmu.Unlock()
	_ = p.send(childMsg{Type: msgPrint, Text: text})
}

// gone is the answer to a call once the gateway stopped answering.
func gone(err error) parentMsg {
	return parentMsg{Status: StatusFailed, Error: "tool call failed: the gateway went away: " + err.Error()}
}

func (p *pipeSink) start(req callRequest) (int, <-chan parentMsg) {
	ch := make(chan parentMsg, 1)
	p.readerOnce.Do(func() { go p.readAnswers() })
	p.mu.Lock()
	if p.readErr != nil {
		err := p.readErr
		p.mu.Unlock()
		ch <- gone(err)
		return 0, ch
	}
	p.nextID++
	id := p.nextID
	p.waiting[id] = ch
	p.mu.Unlock()
	msg := childMsg{Type: msgCall, ID: id, Server: req.server, Member: req.member, Name: req.name, Args: req.args, Try: req.try}
	if err := p.send(msg); err != nil {
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
		ch <- gone(err)
	}
	return id, ch
}

// readAnswers hands each answer on stdin to its call, until stdin ends;
// then every call still waiting gets the gateway's absence as its answer.
func (p *pipeSink) readAnswers() {
	for {
		line, err := readLine(p.r)
		if err != nil {
			p.mu.Lock()
			p.readErr = err
			waiting := p.waiting
			p.waiting = map[int]chan parentMsg{}
			p.mu.Unlock()
			for _, ch := range waiting {
				ch <- gone(err)
			}
			return
		}
		var reply parentMsg
		if json.Unmarshal(line, &reply) != nil || reply.Type != msgResult {
			continue
		}
		p.mu.Lock()
		ch, ok := p.waiting[reply.ID]
		delete(p.waiting, reply.ID)
		p.mu.Unlock()
		if ok {
			ch <- reply
		}
	}
}

// finish reports the outcome.
func (p *pipeSink) finish(res scriptResult) error {
	if res.errKind != "" {
		return p.send(childMsg{Type: msgError, Kind: res.errKind, Message: res.errMsg, Cause: res.cause, Call: res.call})
	}
	return p.send(childMsg{Type: msgDone, Result: res.result, HasResult: res.hasResult})
}
