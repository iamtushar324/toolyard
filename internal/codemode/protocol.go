package codemode

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// The parent and the worker speak newline-delimited JSON over the worker's
// stdin and stdout. The parent opens with one start message; the worker
// then sends prints and call requests, the parent answers each call with a
// result, and the worker ends with done or error and exits. Nothing else
// crosses: no reasons, no credentials, no catalog beyond identifiers.
//
// Sizes are bounded at the source, so no message can approach the line
// limit and leave a side blocked on a pipe: the worker caps a returned
// value and a call's arguments before writing, the parent caps a tool
// result before replying, and a print is cut to one megabyte.

const (
	msgStart  = "start"
	msgCall   = "call"
	msgPrint  = "print"
	msgDone   = "done"
	msgError  = "error"
	msgResult = "result"

	// maxLineBytes bounds one message either way; anything past it is a
	// protocol failure that ends the run.
	maxLineBytes = 8 << 20
	// maxResultBytes caps the JSON of a script's returned value. The
	// response is truncated to about this size anyway.
	maxResultBytes = 1 << 20
	// maxArgsBytes caps the JSON of one call's arguments.
	maxArgsBytes = 4 << 20
	// maxCallResultBytes caps the JSON string of one tool result on its way
	// to the script.
	maxCallResultBytes = 6 << 20
	// maxPrintBytes caps one print() line.
	maxPrintBytes = 1 << 20
	// maxErrorBytes caps the text of an error either side reports: a
	// script's failure message, or a failed tool result the parent relays.
	maxErrorBytes = 64 << 10
	// maxOutputCap is the most Limits.MaxOutputBytes may be set to, so the
	// print log stays well inside the line limit.
	maxOutputCap = 2 << 20
	// maxParallelCalls caps the nested calls one script has in flight at
	// once; more wait in order. defaultParallel is the parallel() limit
	// when the script names none.
	maxParallelCalls = 8
	defaultParallel  = 4
)

// startMsg is the parent's opening message.
type startMsg struct {
	Type    string       `json:"type"`
	Code    string       `json:"code"`
	Servers []wireServer `json:"servers"`
	Limits  Limits       `json:"limits"`
	// Language is LanguageStarlark ("" in an older parent) or
	// LanguageJavaScript.
	Language string `json:"language,omitempty"`
	// MaxParallel caps the calls a script keeps in flight at once
	// (JavaScript's tools.parallel, Starlark's parallel).
	MaxParallel int `json:"maxParallel,omitempty"`
	// MemoryMiB is the worker's memory cap, which sizes the JavaScript
	// heap.
	MemoryMiB int `json:"memoryMiB,omitempty"`
}

// wireServer is one server as the worker binds it: its identifier, the
// member identifiers it may call, and the identifiers it must refuse with
// the given message (names two tools mangle to).
type wireServer struct {
	Ident   string            `json:"ident"`
	Members []string          `json:"members"`
	Refused map[string]string `json:"refused,omitempty"`
}

// childMsg is anything the worker sends.
type childMsg struct {
	Type string `json:"type"`
	// call
	ID     int            `json:"id,omitempty"`
	Server string         `json:"server,omitempty"`
	Member string         `json:"member,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	// Name is a call by name ("Server.tool"), with Server and Member
	// empty: JavaScript's tools.call, Starlark's try_call("...").
	Name string `json:"name,omitempty"`
	// Try asks for the call's outcome as data: a failed or held call does
	// not end the script.
	Try bool `json:"try,omitempty"`
	// print
	Text string `json:"text,omitempty"`
	// done
	Result    json.RawMessage `json:"result,omitempty"`
	HasResult bool            `json:"hasResult,omitempty"`
	// error
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
	// Cause is the structured run's error kind when the script side knows
	// it (ErrorTimeout, ErrorLimit); the parent accepts no other value.
	Cause string `json:"cause,omitempty"`
	// Call names the call (its ID) whose failure, uncaught, ended a
	// JavaScript script, so the run reports that call's outcome
	// (permission_required, say) rather than a script error.
	Call int `json:"call,omitempty"`
}

// parentMsg answers one call: the tool's text (or, for JavaScript, its
// structured content as JSON), or the error that aborts the script.
type parentMsg struct {
	Type  string `json:"type"`
	ID    int    `json:"id"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
	// JSON is the tool's structured content, sent instead of Text to a
	// JavaScript script.
	JSON json.RawMessage `json:"json,omitempty"`
	// Status is the call's status (succeeded, failed,
	// permission_required, deferred, outcome_unknown).
	Status string `json:"status,omitempty"`
	// ApprovalID names the legacy approval a deferred call waits on.
	ApprovalID string `json:"approvalId,omitempty"`
	// DispatchID names the gateway dispatch that made the call ("" when
	// the call never reached one), for audit.mine dispatch_id=… .
	DispatchID string `json:"dispatchId,omitempty"`
}

// cut bounds s to max bytes (on a rune boundary), marking the cut.
func cut(s string, max int, note string) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + note
}

// errLineTooLong is a message past maxLineBytes.
var errLineTooLong = fmt.Errorf("message exceeds the %d MiB line limit", maxLineBytes>>20)

// readLine returns the next newline-terminated message without the newline.
func readLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(out)+len(chunk) > maxLineBytes {
			return nil, errLineTooLong
		}
		out = append(out, chunk...)
		switch {
		case err == nil:
			return out[:len(out)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if len(out) > 0 && err.Error() == "EOF" {
				return nil, fmt.Errorf("unterminated message: %w", err)
			}
			return nil, err
		}
	}
}
