package voice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsTestServer spins up an httptest server that upgrades to a WebSocket and
// hands the resulting *wsSource back to the test. The handler stays alive
// until cleanup so the read loop keeps running.
func wsTestServer(t *testing.T) (*wsSource, *websocket.Conn) {
	t.Helper()
	srcCh := make(chan *wsSource, 1)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		s := newWSSource(conn)
		s.start(r.Context())
		srcCh <- s
		<-stop
		conn.Close(websocket.StatusNormalClosure, "")
	}))

	ctx := context.Background()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() {
		client.Close(websocket.StatusNormalClosure, "")
		close(stop)
		srv.Close()
	})
	return <-srcCh, client
}

func TestWSSource_MuteDropsFrames(t *testing.T) {
	src, client := wsTestServer(t)
	ctx := context.Background()

	// Muted: a binary frame must be drained, never reaching the mic chan.
	src.SetMuted(true)
	if err := client.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatalf("write muted frame: %v", err)
	}
	select {
	case d := <-src.Mic():
		t.Fatalf("muted frame leaked to mic: %v", d)
	case <-time.After(200 * time.Millisecond):
		// good — dropped.
	}

	// Unmuted: the next frame flows through.
	src.SetMuted(false)
	if err := client.Write(ctx, websocket.MessageBinary, []byte{4, 5, 6}); err != nil {
		t.Fatalf("write unmuted frame: %v", err)
	}
	select {
	case d := <-src.Mic():
		if len(d) != 3 || d[0] != 4 {
			t.Fatalf("mic frame = %v, want [4 5 6]", d)
		}
	case <-time.After(time.Second):
		t.Fatal("unmuted frame never reached mic")
	}
}

func TestWSSource_RateCapClosesConn(t *testing.T) {
	src, client := wsTestServer(t)

	// Mute so frames are drained (not buffered) — the rate check runs
	// before the mute check, so the bucket is still charged.
	src.SetMuted(true)

	// Flood ~2.5 MiB well past the 1 MiB burst, in 16 KiB frames (under
	// coder/websocket's 32 KiB per-message read limit, like real mic
	// chunks). Each write gets its own short deadline: once the cap fires
	// the server stops reading and the remaining writes block on
	// backpressure — expected, we just move on and assert the effect.
	frame := make([]byte, 16*1024)
	for i := 0; i < 160; i++ {
		wctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := client.Write(wctx, websocket.MessageBinary, frame)
		cancel()
		if err != nil {
			break
		}
	}

	// The read-rate cap should have fired server-side and surfaced
	// errReadRateExceeded on the source's error channel.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := src.Err(); err != nil {
			if err == errReadRateExceeded {
				return // success
			}
			t.Fatalf("src.Err() = %v, want errReadRateExceeded", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("read-rate cap did not fire within deadline")
}
