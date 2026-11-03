package token

import (
	"bytes"
	"fmt"
	"testing"
)

func TestStringTable(t *testing.T) {
	st := NewStringTable(2048, 256)

	if id := st.Intern(nil); id != NoString {
		t.Fatalf("expected NoString for nil slice, got %d", id)
	}
	if b := st.Bytes(NoString); b != nil {
		t.Fatalf("expected nil bytes for NoString, got %v", b)
	}

	words := [][]byte{
		[]byte("let"),
		[]byte("mutable"),
		[]byte("rec"),
		[]byte("match"),
		[]byte("MailboxProcessor"),
		[]byte("x"),
		[]byte("y"),
	}

	ids := make([]StringID, len(words))
	for i, w := range words {
		ids[i] = st.Intern(w)
		if ids[i] == NoString {
			t.Fatalf("unexpected NoString for %q", w)
		}
	}

	// Verify deduplication and byte retrieval
	for i, w := range words {
		dup := st.Intern(w)
		if dup != ids[i] {
			t.Fatalf("expected identical StringID %d for %q, got %d", ids[i], w, dup)
		}
		if !bytes.Equal(st.Bytes(ids[i]), w) {
			t.Fatalf("expected %q, got %q", w, st.Bytes(ids[i]))
		}
		if st.String(ids[i]) != string(w) {
			t.Fatalf("expected string %q, got %q", w, st.String(ids[i]))
		}
	}

	// Stress grow
	for i := 0; i < 500; i++ {
		s := fmt.Sprintf("ident_%d", i)
		id := st.InternString(s)
		if st.String(id) != s {
			t.Fatalf("grow mismatch for %q: got %q", s, st.String(id))
		}
	}

	// Verify zero allocations on existing lookup
	probe := []byte("MailboxProcessor")
	allocs := testing.AllocsPerRun(100, func() {
		_ = st.Intern(probe)
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op on existing Intern lookup, got %v", allocs)
	}
}

func BenchmarkInternExisting(b *testing.B) {
	st := NewStringTable(4096, 512)
	sample := []byte("computePipelineTransform")
	_ = st.Intern(sample)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = st.Intern(sample)
	}
}
