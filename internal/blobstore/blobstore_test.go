package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// pattern returns n deterministic pseudo-random bytes that differ per seed.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed) + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

type testStore struct {
	name string
	s    Store
}

// testStores returns a Local store and a database store. The database store
// runs on the engine storetest picks: Postgres in the Postgres CI job, SQLite
// otherwise (its SQL is portable, and migration 0040 creates the tables on
// both engines).
func testStores(t *testing.T) []testStore {
	t.Helper()
	l, err := NewLocal(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	return []testStore{{"local", l}, {"db-" + storetest.Engine().String(), NewPostgres(storetest.Open(t).DB)}}
}

func readAll(t *testing.T, s Store, sha string) []byte {
	t.Helper()
	b, err := s.Open(context.Background(), sha)
	if err != nil {
		t.Fatalf("open %s: %v", sha[:12], err)
	}
	defer b.Close()
	data, err := io.ReadAll(b)
	if err != nil {
		t.Fatalf("read %s: %v", sha[:12], err)
	}
	return data
}

// chunkRows counts a blob's chunk rows (database store only).
func chunkRows(t *testing.T, s Store, sha string) int {
	t.Helper()
	var n int
	if err := s.(*dbStore).db.QueryRow(`SELECT COUNT(*) FROM inbox_blob_chunks WHERE sha256 = ?`, sha).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPutOpenExistsDelete(t *testing.T) {
	ctx := context.Background()
	for _, ts := range testStores(t) {
		t.Run(ts.name, func(t *testing.T) {
			s := ts.s
			data := pattern(5000, 1)
			sha := hexSum(data)
			if ok, err := s.Exists(ctx, sha); err != nil || ok {
				t.Fatalf("exists before put: %v %v", ok, err)
			}
			if _, err := s.Open(ctx, sha); !errors.Is(err, ErrNotFound) {
				t.Fatalf("open before put: %v, want ErrNotFound", err)
			}
			created, err := s.Put(ctx, sha, bytes.NewReader(data), int64(len(data)))
			if err != nil || !created {
				t.Fatalf("first put: created=%v err=%v", created, err)
			}
			created, err = s.Put(ctx, sha, bytes.NewReader(data), int64(len(data)))
			if err != nil || created {
				t.Fatalf("second put: created=%v err=%v, want created=false", created, err)
			}
			if ok, err := s.Exists(ctx, sha); err != nil || !ok {
				t.Fatalf("exists after put: %v %v", ok, err)
			}
			b, err := s.Open(ctx, sha)
			if err != nil {
				t.Fatal(err)
			}
			if b.Size() != int64(len(data)) {
				t.Fatalf("size %d, want %d", b.Size(), len(data))
			}
			_ = b.Close()
			if got := readAll(t, s, sha); !bytes.Equal(got, data) {
				t.Fatal("the bytes changed on a round trip")
			}
			if err := s.Delete(ctx, sha); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.Exists(ctx, sha); err != nil || ok {
				t.Fatalf("exists after delete: %v %v", ok, err)
			}
			if _, err := s.Open(ctx, sha); !errors.Is(err, ErrNotFound) {
				t.Fatalf("open after delete: %v, want ErrNotFound", err)
			}
			if err := s.Delete(ctx, sha); err != nil {
				t.Fatalf("deleting a missing blob: %v", err)
			}
			if _, isDB := s.(*dbStore); isDB && chunkRows(t, s, sha) != 0 {
				t.Fatal("delete left chunk rows")
			}

			// An empty blob is a blob too.
			empty := hexSum(nil)
			if created, err := s.Put(ctx, empty, bytes.NewReader(nil), 0); err != nil || !created {
				t.Fatalf("empty put: created=%v err=%v", created, err)
			}
			if got := readAll(t, s, empty); len(got) != 0 {
				t.Fatalf("empty blob read %d bytes", len(got))
			}
		})
	}
}

func TestPutRefusesWrongSizesAndNames(t *testing.T) {
	ctx := context.Background()
	for _, ts := range testStores(t) {
		t.Run(ts.name, func(t *testing.T) {
			s := ts.s
			data := pattern(3000, 2)
			sha := hexSum(data)
			if _, err := s.Put(ctx, sha, bytes.NewReader(data[:len(data)-1]), int64(len(data))); err == nil {
				t.Fatal("a short body was stored")
			}
			if _, err := s.Put(ctx, sha, bytes.NewReader(append(append([]byte{}, data...), 'x')), int64(len(data))); err == nil {
				t.Fatal("a long body was stored")
			}
			if ok, _ := s.Exists(ctx, sha); ok {
				t.Fatal("a failed put left a blob")
			}
			if _, isDB := s.(*dbStore); isDB && chunkRows(t, s, sha) != 0 {
				t.Fatal("a failed put left chunk rows")
			}
			for _, bad := range []string{"../../etc/passwd", strings.ToUpper(sha), sha[:63], ""} {
				if _, err := s.Put(ctx, bad, bytes.NewReader(data), int64(len(data))); err == nil {
					t.Errorf("put %q accepted", bad)
				}
				if _, err := s.Open(ctx, bad); !errors.Is(err, ErrNotFound) {
					t.Errorf("open %q: %v, want ErrNotFound", bad, err)
				}
				if ok, err := s.Exists(ctx, bad); ok || err != nil {
					t.Errorf("exists %q: %v %v", bad, ok, err)
				}
				if err := s.Delete(ctx, bad); err != nil {
					t.Errorf("delete %q: %v", bad, err)
				}
			}
			if created, err := s.Put(ctx, sha, bytes.NewReader(data), int64(len(data))); err != nil || !created {
				t.Fatalf("put after the failures: created=%v err=%v", created, err)
			}
			if got := readAll(t, s, sha); !bytes.Equal(got, data) {
				t.Fatal("the bytes changed on a round trip")
			}
		})
	}
}

// Several writers storing the same blob at once all succeed; the database
// store reports created to exactly one of them.
func TestConcurrentIdenticalPut(t *testing.T) {
	ctx := context.Background()
	for _, ts := range testStores(t) {
		t.Run(ts.name, func(t *testing.T) {
			s := ts.s
			data := pattern(2*ChunkSize+777, 3)
			sha := hexSum(data)
			const writers = 4
			var wg sync.WaitGroup
			var createdN atomic.Int32
			errs := make(chan error, writers)
			start := make(chan struct{})
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					created, err := s.Put(ctx, sha, bytes.NewReader(data), int64(len(data)))
					if err != nil {
						errs <- err
						return
					}
					if created {
						createdN.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("concurrent put: %v", err)
			}
			if createdN.Load() < 1 {
				t.Fatal("no writer stored the blob")
			}
			// Local renames, so two writers that both miss the file may both
			// report created (as toolyard always did); the bytes are the same.
			if _, isDB := s.(*dbStore); isDB {
				if createdN.Load() != 1 {
					t.Fatalf("%d writers report created, want 1", createdN.Load())
				}
				if n := chunkRows(t, s, sha); n != 3 {
					t.Fatalf("%d chunk rows, want 3", n)
				}
			}
			if got := readAll(t, s, sha); !bytes.Equal(got, data) {
				t.Fatal("the bytes changed after concurrent puts")
			}
		})
	}
}

func TestSeekAndReadAcrossChunks(t *testing.T) {
	ctx := context.Background()
	for _, ts := range testStores(t) {
		t.Run(ts.name, func(t *testing.T) {
			s := ts.s
			data := pattern(2*ChunkSize+1234, 4)
			sha := hexSum(data)
			size := int64(len(data))
			if _, err := s.Put(ctx, sha, bytes.NewReader(data), size); err != nil {
				t.Fatal(err)
			}
			b, err := s.Open(ctx, sha)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			check := func(off int64, n int) {
				t.Helper()
				if pos, err := b.Seek(off, io.SeekStart); err != nil || pos != off {
					t.Fatalf("seek %d: %d %v", off, pos, err)
				}
				got := make([]byte, n)
				if _, err := io.ReadFull(b, got); err != nil {
					t.Fatalf("read %d bytes at %d: %v", n, off, err)
				}
				if !bytes.Equal(got, data[off:off+int64(n)]) {
					t.Fatalf("wrong bytes at %d", off)
				}
			}
			check(ChunkSize-10, 20)  // across the first chunk boundary
			check(2*ChunkSize-1, 2)  // the last byte of chunk 1, the first of chunk 2
			check(0, 5)              // back to chunk 0
			check(size-7, 7)         // the tail
			check(ChunkSize, 3*1024) // the start of a chunk
			if pos, err := b.Seek(-100, io.SeekEnd); err != nil || pos != size-100 {
				t.Fatalf("seek from the end: %d %v", pos, err)
			}
			if pos, err := b.Seek(50, io.SeekCurrent); err != nil || pos != size-50 {
				t.Fatalf("seek from the current position: %d %v", pos, err)
			}
			rest, err := io.ReadAll(b)
			if err != nil || !bytes.Equal(rest, data[size-50:]) {
				t.Fatalf("read the rest: %d bytes, %v", len(rest), err)
			}
			if n, err := b.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("read at the end: %d %v, want 0 EOF", n, err)
			}
			if _, err := b.Seek(-1, io.SeekStart); err == nil {
				t.Fatal("a negative position was accepted")
			}
		})
	}
}

// http.ServeContent serves Range requests from a three-chunk blob, the way
// the Inbox blob route does.
func TestServeContentRangeFromChunks(t *testing.T) {
	ctx := context.Background()
	for _, ts := range testStores(t) {
		t.Run(ts.name, func(t *testing.T) {
			s := ts.s
			data := pattern(2*ChunkSize+4321, 5)
			sha := hexSum(data)
			size := len(data)
			if _, err := s.Put(ctx, sha, bytes.NewReader(data), int64(size)); err != nil {
				t.Fatal(err)
			}
			serve := func(rng string) *httptest.ResponseRecorder {
				t.Helper()
				b, err := s.Open(ctx, sha)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				req := httptest.NewRequest(http.MethodGet, "/blob", nil)
				if rng != "" {
					req.Header.Set("Range", rng)
				}
				rec := httptest.NewRecorder()
				http.ServeContent(rec, req, "", time.Time{}, b)
				return rec
			}
			from, to := ChunkSize-100, 2*ChunkSize+50 // touches all three chunks
			rec := serve(fmt.Sprintf("bytes=%d-%d", from, to))
			if rec.Code != http.StatusPartialContent {
				t.Fatalf("range: %d, want 206", rec.Code)
			}
			if got, want := rec.Header().Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", from, to, size); got != want {
				t.Fatalf("Content-Range %q, want %q", got, want)
			}
			if !bytes.Equal(rec.Body.Bytes(), data[from:to+1]) {
				t.Fatalf("range body: %d bytes, not the requested slice", rec.Body.Len())
			}
			rec = serve("bytes=-10")
			if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), data[size-10:]) {
				t.Fatalf("suffix range: %d, %d bytes", rec.Code, rec.Body.Len())
			}
			rec = serve("")
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
				t.Fatalf("whole blob: %d, %d bytes", rec.Code, rec.Body.Len())
			}
		})
	}
}

// Chunk rows without an object row (an interrupted write) are replaced by
// the next Put, and a blob with a missing chunk fails its read instead of
// returning short or wrong bytes.
func TestDBStoreStrayAndMissingChunks(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	s := NewPostgres(db.DB)
	data := pattern(2*ChunkSize+10, 6)
	sha := hexSum(data)
	if _, err := db.Exec(`INSERT INTO inbox_blob_chunks(sha256, seq, data) VALUES (?, ?, ?)`, sha, 7, []byte("stray")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Exists(ctx, sha); ok {
		t.Fatal("chunks without an object row must not count as a blob")
	}
	if created, err := s.Put(ctx, sha, bytes.NewReader(data), int64(len(data))); err != nil || !created {
		t.Fatalf("put over stray chunks: created=%v err=%v", created, err)
	}
	if n := chunkRows(t, s, sha); n != 3 {
		t.Fatalf("%d chunk rows, want 3 (the stray one replaced)", n)
	}
	if got := readAll(t, s, sha); !bytes.Equal(got, data) {
		t.Fatal("the bytes changed on a round trip")
	}
	if _, err := db.Exec(`DELETE FROM inbox_blob_chunks WHERE sha256 = ? AND seq = ?`, sha, 1); err != nil {
		t.Fatal(err)
	}
	b, err := s.Open(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := io.ReadAll(b); err == nil {
		t.Fatal("a blob with a missing chunk read without an error")
	}
}

func TestLocalLayout(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "blobs")
	s, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("blob directory: %v %v", st, err)
	}
	data := pattern(100, 7)
	sha := hexSum(data)
	if _, err := s.Put(ctx, sha, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, sha))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("blob file dir/<sha256> with mode 0600: %v %v", st, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("%d files in the blob directory, want only the blob (no temporary file left)", len(entries))
	}
	b, err := s.Open(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(interface{ Stat() (os.FileInfo, error) }); !ok {
		t.Fatal("a local blob should keep the file's Stat")
	}
	_ = b.Close()

	a, ok := s.(FileAdopter)
	if !ok {
		t.Fatal("Local should adopt files")
	}
	other := pattern(50, 8)
	tmp := filepath.Join(dir, "upload-1")
	if err := os.WriteFile(tmp, other, 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := a.AdoptFile(ctx, hexSum(other), tmp); err != nil || !created {
		t.Fatalf("adopt: created=%v err=%v", created, err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the adopted file should have moved")
	}
	if got := readAll(t, s, hexSum(other)); !bytes.Equal(got, other) {
		t.Fatal("adopted bytes differ")
	}
	tmp2 := filepath.Join(dir, "upload-2")
	if err := os.WriteFile(tmp2, other, 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := a.AdoptFile(ctx, hexSum(other), tmp2); err != nil || created {
		t.Fatalf("adopt an existing blob: created=%v err=%v", created, err)
	}
	if _, err := os.Stat(tmp2); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("adopting an existing blob should remove the temporary file")
	}
	outside := filepath.Join(t.TempDir(), "upload-3")
	if err := os.WriteFile(outside, other, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AdoptFile(ctx, hexSum(other), outside); err == nil {
		t.Fatal("a file outside the blob directory was adopted")
	}
}

func TestForDB(t *testing.T) {
	dir := t.TempDir()
	s, err := ForDB(nil, filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*local); !ok {
		t.Fatalf("a nil db should get Local, got %T", s)
	}
	s, err = ForDB(storetest.Open(t), filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	if storetest.Engine().IsPostgres() {
		if _, ok := s.(*dbStore); !ok {
			t.Fatalf("Postgres should get the database store, got %T", s)
		}
	} else if _, ok := s.(*local); !ok {
		t.Fatalf("SQLite should get Local, got %T", s)
	}
}

// genReader yields n pseudo-random bytes without ever holding them, and
// calls tick (when set) at every Read.
type genReader struct {
	left int64
	x    uint32
	tick func()
}

func (g *genReader) Read(p []byte) (int, error) {
	if g.tick != nil {
		g.tick()
	}
	if g.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > g.left {
		p = p[:g.left]
	}
	for i := range p {
		g.x = g.x*1664525 + 1013904223
		p[i] = byte(g.x >> 24)
	}
	g.left -= int64(len(p))
	return len(p), nil
}

// heapRise runs fn with the collector at GOGC=10, so the heap in use tracks
// live memory rather than garbage, and returns how far HeapAlloc rose above
// its level at the start. It samples every 5 ms and whenever fn calls tick.
func heapRise(fn func(tick func())) uint64 {
	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	peak.Store(base)
	tick := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		for {
			old := peak.Load()
			if m.HeapAlloc <= old || peak.CompareAndSwap(old, m.HeapAlloc) {
				return
			}
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tk := time.NewTicker(5 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				tick()
			}
		}
	}()
	fn(tick)
	tick()
	close(stop)
	<-done
	return peak.Load() - base
}

// A 50 MB upload streams into Postgres without the whole file in memory
// (TEC-1636 acceptance), and reads back the same way.
//
// The bound is 24 MiB of heap growth. Put holds one 1 MiB read buffer, and
// pgx encodes each chunk's INSERT into fresh buffers (its parameter buffer
// and its wire buffer, about 2-3 MiB per chunk, garbage right after). With
// GOGC=10 the heap in use stays within a few MiB of the live set, so 24 MiB
// leaves room for GC pacing on a loaded CI runner and is still far below
// the 50 MiB that a whole-file buffer would add. runtime.MemStats.TotalAlloc
// is not a usable measure here: those per-chunk buffers add up to well over
// 100 MiB of short-lived allocations for 50 chunks, though never more than
// a few MiB at once.
func TestPostgresPutStreams50MiB(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	s := NewPostgres(storetest.Open(t).DB)
	const size = 50 << 20
	const bound = 24 << 20
	h := sha256.New()
	if _, err := io.Copy(h, &genReader{left: size}); err != nil {
		t.Fatal(err)
	}
	sha := hex.EncodeToString(h.Sum(nil))

	var created bool
	var err error
	rise := heapRise(func(tick func()) {
		created, err = s.Put(ctx, sha, &genReader{left: size, tick: tick}, size)
	})
	if err != nil || !created {
		t.Fatalf("put 50 MiB: created=%v err=%v", created, err)
	}
	t.Logf("the heap rose %.1f MiB during a 50 MiB Put", float64(rise)/(1<<20))
	if rise >= bound {
		t.Fatalf("the heap rose %d bytes during a 50 MiB Put; want < %d (the blob must stream, not sit in memory)", rise, bound)
	}
	if n := chunkRows(t, s, sha); n != 50 {
		t.Fatalf("%d chunk rows, want 50", n)
	}

	var got string
	rise = heapRise(func(func()) {
		b, oerr := s.Open(ctx, sha)
		if oerr != nil {
			err = oerr
			return
		}
		defer b.Close()
		rh := sha256.New()
		if _, err = io.Copy(rh, b); err == nil {
			got = hex.EncodeToString(rh.Sum(nil))
		}
	})
	if err != nil || got != sha {
		t.Fatalf("read back: %v (sha256 %s, want %s)", err, got, sha)
	}
	t.Logf("the heap rose %.1f MiB while reading the blob back", float64(rise)/(1<<20))
	if rise >= bound {
		t.Fatalf("the heap rose %d bytes while reading 50 MiB back; want < %d", rise, bound)
	}
}
