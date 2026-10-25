package codemode

// sliceMemory is a linear memory backed by a Go slice: the fallback when
// no address space can be reserved, and the only kind on platforms
// without mmap.
type sliceMemory struct{ buf []byte }

func (m *sliceMemory) Reallocate(size uint64) []byte {
	if size > uint64(len(m.buf)) {
		grown := make([]byte, size)
		copy(grown, m.buf)
		m.buf = grown
	}
	return m.buf[:size:size]
}

func (m *sliceMemory) Free() { m.buf = nil }
