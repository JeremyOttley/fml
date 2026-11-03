// Package token defines the compact lexical token representation and contiguous
// string interning table for the FML compiler.
//
// Jaime says: Stop allocating a heap string for every identifier in the source
// file. Store bytes in one flat buffer, hash the byte slice in place, and pass
// around a 32-bit StringID. Cache lines love contiguous bytes; the garbage
// collector loves having nothing to scan.
package token

import "bytes"

// StringID is a 1-based index into a StringTable's span array.
// Jaime says: Pointers are 64 bits and make the GC work for a living.
// A uint32 index is half the size and twice as fast.
type StringID uint32

// NoString represents the zero/empty StringID.
const NoString StringID = 0

// spanEntry records the byte offset and length of an interned symbol inside
// StringTable.buf.
type spanEntry struct {
	offset uint32
	length uint32
}

// StringTable stores unique byte sequences in a single contiguous byte arena
// with an open-addressed FNV-1a index table.
//
// Jaime says: One byte slice for raw characters, one slice of 8-byte spans, and
// a flat power-of-two hash table of uint32 indices. Zero pointer chasing.
type StringTable struct {
	buf     []byte
	spans   []spanEntry
	buckets []StringID
	count   uint32
}

// NewStringTable pre-allocates a StringTable with capacities tuned for typical
// compilation units so lexing and parsing run without resizing.
func NewStringTable(byteCap, symbolCap int) *StringTable {
	if byteCap < 1024 {
		byteCap = 1024
	}
	if symbolCap < 128 {
		symbolCap = 128
	}
	bucketCap := nextPow2(uint32(symbolCap * 2))
	st := &StringTable{
		buf:     make([]byte, 0, byteCap),
		spans:   make([]spanEntry, 1, symbolCap+1), // index 0 reserved for NoString
		buckets: make([]StringID, bucketCap),
	}
	return st
}

func nextPow2(v uint32) uint32 {
	if v < 16 {
		return 16
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	return v + 1
}

func fnv1a(b []byte) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(b); i++ {
		h ^= uint32(b[i])
		h *= 16777619
	}
	return h
}

// Intern returns a canonical StringID for the byte slice b.
// Lookup of an already-interned slice performs zero heap allocations.
func (st *StringTable) Intern(b []byte) StringID {
	if len(b) == 0 {
		return NoString
	}
	if (st.count+1)*4 >= uint32(len(st.buckets))*3 {
		st.grow()
	}
	mask := uint32(len(st.buckets) - 1)
	idx := fnv1a(b) & mask
	for {
		id := st.buckets[idx]
		if id == NoString {
			break
		}
		sp := st.spans[id]
		if int(sp.length) == len(b) && bytes.Equal(st.buf[sp.offset:sp.offset+sp.length], b) {
			return id
		}
		idx = (idx + 1) & mask
	}

	off := uint32(len(st.buf))
	st.buf = append(st.buf, b...)
	newID := StringID(len(st.spans))
	st.spans = append(st.spans, spanEntry{
		offset: off,
		length: uint32(len(b)),
	})
	st.buckets[idx] = newID
	st.count++
	return newID
}

// InternString interns a Go string by converting it to bytes only when needed.
func (st *StringTable) InternString(s string) StringID {
	if len(s) == 0 {
		return NoString
	}
	return st.Intern([]byte(s))
}

// Bytes returns the underlying byte slice for id. The returned slice aliases
// the arena buffer and must not be mutated by callers.
func (st *StringTable) Bytes(id StringID) []byte {
	if id == NoString || int(id) >= len(st.spans) {
		return nil
	}
	sp := st.spans[id]
	return st.buf[sp.offset : sp.offset+sp.length]
}

// String returns a Go string for id (for diagnostics and formatting).
func (st *StringTable) String(id StringID) string {
	b := st.Bytes(id)
	if len(b) == 0 {
		return ""
	}
	return string(b)
}

// Len returns the number of unique interned symbols.
func (st *StringTable) Len() int {
	return int(st.count)
}

func (st *StringTable) grow() {
	newCap := uint32(len(st.buckets)) * 2
	if newCap == 0 {
		newCap = 256
	}
	newBuckets := make([]StringID, newCap)
	mask := newCap - 1
	for id := StringID(1); int(id) < len(st.spans); id++ {
		sp := st.spans[id]
		b := st.buf[sp.offset : sp.offset+sp.length]
		idx := fnv1a(b) & mask
		for newBuckets[idx] != NoString {
			idx = (idx + 1) & mask
		}
		newBuckets[idx] = id
	}
	st.buckets = newBuckets
}
