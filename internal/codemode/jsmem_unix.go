//go:build linux || darwin

package codemode

import (
	"context"
	"os"

	"github.com/tetratelabs/wazero/experimental"
	"golang.org/x/sys/unix"
)

// The JavaScript engine's WebAssembly memory lives outside the Go heap.
// wazero's default backs a module's linear memory with a Go []byte that
// it copies to grow, which would count against the worker's Go memory
// limit and, while growing, hold two copies. Instead the whole maximum is
// reserved up front as inaccessible address space (PROT_NONE, which costs
// nothing and does not count against RLIMIT_DATA) and made readable and
// writable as QuickJS grows its heap, in place. Past the worker's memory
// cap that mprotect fails, the module's memory.grow fails, QuickJS's
// malloc returns NULL, and the script sees a catchable "out of memory"
// error instead of the worker dying. mmap, mprotect and munmap are all
// allowed by the worker's seccomp filter.

// withJSMemory makes modules instantiated with ctx use mmapMemory.
func withJSMemory(ctx context.Context) context.Context {
	return experimental.WithMemoryAllocator(ctx, experimental.MemoryAllocatorFunc(newJSMemory))
}

func newJSMemory(capacity, max uint64) experimental.LinearMemory {
	page := uint64(os.Getpagesize())
	size := (max + page - 1) / page * page
	if size == 0 || size > 1<<32 {
		return &sliceMemory{}
	}
	mem, err := unix.Mmap(-1, 0, int(size), unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON|unix.MAP_NORESERVE)
	if err != nil {
		return &sliceMemory{}
	}
	return &mmapMemory{mem: mem, page: int(page)}
}

// mmapMemory is a reserved range made accessible as it grows.
type mmapMemory struct {
	mem       []byte // the whole reservation
	page      int
	committed int // bytes made readable and writable
}

func (m *mmapMemory) Reallocate(size uint64) []byte {
	if size > uint64(len(m.mem)) {
		return nil
	}
	want := (int(size) + m.page - 1) / m.page * m.page
	if want > m.committed {
		if err := unix.Mprotect(m.mem[m.committed:want], unix.PROT_READ|unix.PROT_WRITE); err != nil {
			return nil
		}
		m.committed = want
	}
	return m.mem[:size:size]
}

func (m *mmapMemory) Free() {
	if m.mem != nil {
		_ = unix.Munmap(m.mem)
		m.mem = nil
	}
}
