package types

import (
	"testing"
	"unsafe"
)

func TestTypeArenaUnionFind(t *testing.T) {
	if sz := unsafe.Sizeof(TypeNode{}); sz > 24 {
		t.Fatalf("TypeNode size %d exceeds 24 bytes", sz)
	}

	ta := NewTypeArena(512, nil)
	v1 := ta.FreshVar()
	v2 := ta.FreshVar()
	v3 := ta.FreshVar()

	ta.Union(v1, v2)
	ta.Union(v2, v3)
	ta.Union(v3, TypeInt)

	if got := ta.Find(v1); got != TypeInt {
		t.Fatalf("expected v1 to resolve to TypeInt (%d), got %d", TypeInt, got)
	}
	// After path compression, v1.Parent must point directly to TypeInt
	if ta.Nodes[v1].Parent != TypeInt {
		t.Fatalf("expected path compression on v1.Parent, got %d", ta.Nodes[v1].Parent)
	}

	allocs := testing.AllocsPerRun(100, func() {
		_ = ta.Find(v1)
		_ = ta.Union(v1, v2)
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op in Find and Union, got %v", allocs)
	}
}
