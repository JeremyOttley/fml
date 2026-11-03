package ast

import (
	"testing"
	"unsafe"

	"fml/internal/token"
)

func TestASTNodeSizeAndArenaZeroAlloc(t *testing.T) {
	sz := unsafe.Sizeof(Node{})
	if sz > 32 {
		t.Fatalf("ast.Node size %d exceeds 32-byte limit", sz)
	}

	st := token.NewStringTable(1024, 128)
	arena := NewArena(1024, 1024, st)

	// Verify index 0 is NoNode
	if len(arena.Nodes) != 1 || arena.Nodes[0].Kind != KindInvalid {
		t.Fatalf("expected index 0 to be reserved NoNode")
	}

	lhs := arena.Add(Node{Kind: KindInt, Data: 10})
	rhs := arena.Add(Node{Kind: KindInt, Data: 20})
	sum := arena.Add(Node{
		Kind:  KindInfix,
		Op:    uint16(token.TokenPlus),
		Left:  uint32(lhs),
		Right: uint32(rhs),
	})

	if arena.Get(sum).Left != uint32(lhs) || arena.Get(sum).Right != uint32(rhs) {
		t.Fatalf("unexpected children on sum node: %+v", arena.Get(sum))
	}

	start, count := arena.AddExtra([]NodeID{lhs, rhs, sum})
	children := arena.ExtraSlice(start, count)
	if len(children) != 3 || NodeID(children[2]) != sum {
		t.Fatalf("unexpected extra slice: %v", children)
	}

	// Verify 0 allocations when adding to pre-allocated Arena
	allocs := testing.AllocsPerRun(100, func() {
		if len(arena.Nodes) >= cap(arena.Nodes)-2 {
			arena.Nodes = arena.Nodes[:1]
		}
		_ = arena.Add(Node{Kind: KindInt, Data: 42})
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op on pre-allocated Arena.Add, got %v", allocs)
	}
}
