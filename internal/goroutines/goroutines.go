// Package goroutines wraps `go func()` with panic recovery + crash dumps.
//
// One panic in a background goroutine used to take the whole gateway
// down. Now: the recover writes a crash dump and logs a structured
// `goroutine.panic` event; the process keeps serving everything else.
//
// Two flavors:
//
//	goroutines.Go("scanner", fn)            // one-shot; logs & exits on panic
//	goroutines.Supervise(ctx, "scanner", …) // restart with backoff
package goroutines

import (
	"context"
	"log/slog"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/crashdump"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// Go launches fn in a new goroutine with panic recovery. On panic it writes
// a crash dump, logs at error level, and returns (does not restart).
func Go(name string, fn func()) {
	go func() {
		defer recoverAndLog(name, nil)
		fn()
	}()
}

// Supervise launches fn in a goroutine; if it panics or returns an error,
// it's restarted with exponential backoff (capped at maxBackoff). Stops
// when ctx is cancelled. fn should respect ctx and return promptly on
// cancellation; otherwise Supervise can't tell shutdown from work.
func Supervise(ctx context.Context, name string, maxBackoff time.Duration, fn func(context.Context) error) {
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	go func() {
		backoff := 500 * time.Millisecond
		for ctx.Err() == nil {
			func() {
				defer recoverAndLog(name, map[string]string{"supervised": "true"})
				if err := fn(ctx); err != nil && ctx.Err() == nil {
					logx.For("goroutines").Error("supervised exited with error",
						"name", name, "err", err.Error())
				}
			}()
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}()
}

func recoverAndLog(name string, extra map[string]string) {
	r := recover()
	if r == nil {
		return
	}
	path := crashdump.Record("goroutine:"+name, r, extra)
	logx.For("goroutines").LogAttrs(context.Background(), slog.LevelError, "goroutine panic",
		slog.String("name", name),
		slog.Any("panic", r),
		slog.String("crash_dump", path),
	)
}
