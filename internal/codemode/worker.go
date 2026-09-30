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
// message, run the script, report, exit.

// MemoryEnv carries the memory cap, in MiB, from the parent to the worker.
// It is the one setting the worker needs before it reads anything.
const MemoryEnv = "TOOLYARD_CODEMODE_MEMORY_MIB"

// WorkerMain runs one script as the gateway's child process: stdin carries
// the start message and call replies, stdout the calls, prints and the
// outcome. The memory cap is applied before anything is read. The exit
// code is 0 once the outcome was reported, 1 on a setup or protocol
// failure (the parent reports either as a worker failure).
func WorkerMain(stdin io.Reader, stdout, stderr io.Writer) int {
	mib := 512
	if v, err := strconv.Atoi(os.Getenv(MemoryEnv)); err == nil && v > 0 {
		mib = v
	}
	if err := applyMemoryCap(mib); err != nil {
		fmt.Fprintln(stderr, "codemode-worker: memory cap:", err)
		return 1
	}
	debug.SetMemoryLimit(int64(mib) << 20 * 3 / 4)

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
	p := &pipeSink{r: r, w: stdout, limits: start.Limits}
	res := runScript(ctx, start, p, fmt.Sprintf("limit %s", start.Limits.ScriptTimeout))
	if err := p.finish(res); err != nil {
		fmt.Fprintln(stderr, "codemode-worker: report:", err)
		return 1
	}
	return 0
}

// pipeSink is the parent at the other end of stdin and stdout.
type pipeSink struct {
	r      *bufio.Reader
	w      io.Writer
	limits Limits

	mu     sync.Mutex
	nextID int
	// sent counts print bytes; past the limit one crossing print goes out
	// (the parent turns it into the truncation note) and the rest stay
	// here.
	sent   int
	capped bool
}

func (p *pipeSink) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = p.w.Write(append(b, '\n'))
	return err
}

func (p *pipeSink) print(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.capped {
		return
	}
	if p.sent+len(text) > p.limits.MaxOutputBytes {
		p.capped = true
		if len(text) > 1024 {
			text = text[:1024]
		}
	}
	p.sent += len(text) + 1
	_ = p.send(childMsg{Type: msgPrint, Text: text})
}

func (p *pipeSink) call(server, member string, args map[string]any) parentMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	id := p.nextID
	if err := p.send(childMsg{Type: msgCall, ID: id, Server: server, Member: member, Args: args}); err != nil {
		return parentMsg{Error: "tool call failed: the gateway went away: " + err.Error()}
	}
	for {
		line, err := readLine(p.r)
		if err != nil {
			return parentMsg{Error: "tool call failed: the gateway went away: " + err.Error()}
		}
		var reply parentMsg
		if json.Unmarshal(line, &reply) != nil || reply.Type != msgResult || reply.ID != id {
			continue
		}
		return reply
	}
}

// finish reports the outcome.
func (p *pipeSink) finish(res scriptResult) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if res.errKind != "" {
		return p.send(childMsg{Type: msgError, Kind: res.errKind, Message: res.errMsg})
	}
	return p.send(childMsg{Type: msgDone, Result: res.result, HasResult: res.hasResult})
}
