package codemode

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
)

// The parent and the worker speak newline-delimited JSON over the worker's
// stdin and stdout. The parent opens with one start message; the worker
// then sends prints and call requests, the parent answers each call with a
// result, and the worker ends with done or error and exits. Nothing else
// crosses: no reasons, no credentials, no catalog beyond identifiers.

const (
	msgStart  = "start"
	msgCall   = "call"
	msgPrint  = "print"
	msgDone   = "done"
	msgError  = "error"
	msgResult = "result"

	// maxLineBytes bounds one message either way. A tool result or a call's
	// arguments can be large; anything past this is a protocol failure.
	maxLineBytes = 64 << 20
)

// startMsg is the parent's opening message.
type startMsg struct {
	Type    string       `json:"type"`
	Code    string       `json:"code"`
	Servers []wireServer `json:"servers"`
	Limits  Limits       `json:"limits"`
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
	// print
	Text string `json:"text,omitempty"`
	// done
	Result    json.RawMessage `json:"result,omitempty"`
	HasResult bool            `json:"hasResult,omitempty"`
	// error
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
}

// parentMsg answers one call: the tool's text, or the error that aborts the
// script.
type parentMsg struct {
	Type  string `json:"type"`
	ID    int    `json:"id"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// errLineTooLong is a message past maxLineBytes.
var errLineTooLong = errors.New("message exceeds the 64 MiB line limit")

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
